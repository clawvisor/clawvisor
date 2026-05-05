package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

const (
	redisSecretVerdictPrefix      = "clawvisor:runtime:secret_verdict:"
	redisSecretValueVerdictPrefix = "clawvisor:runtime:secret_value_verdict:"
	redisCapturedSecretPrefix     = "clawvisor:runtime:captured_secret:"
	redisCapturedSecretLockPrefix = "clawvisor:runtime:captured_secret_lock:"
	sharedRuntimeCacheTTL         = 24 * time.Hour
)

func (s *Server) sharedSecretVerdictGet(key string) (adjudicationVerdict, bool) {
	if s == nil || s.redisClient == nil || key == "" {
		return adjudicationVerdict{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := s.redisClient.Get(ctx, redisSecretVerdictPrefix+key).Bytes()
	if err != nil {
		return adjudicationVerdict{}, false
	}
	var verdict adjudicationVerdict
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return adjudicationVerdict{}, false
	}
	return verdict, true
}

func (s *Server) sharedSecretVerdictSet(key string, verdict adjudicationVerdict) {
	if s == nil || s.redisClient == nil || key == "" {
		return
	}
	raw, err := json.Marshal(verdict)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.redisClient.Set(ctx, redisSecretVerdictPrefix+key, raw, sharedRuntimeCacheTTL).Err()
}

func (s *Server) sharedSecretValueVerdictGet(host, value string) (adjudicationVerdict, bool) {
	key := s.sharedSecretValueVerdictKey(host, value)
	if key == "" || s.redisClient == nil {
		return adjudicationVerdict{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := s.redisClient.Get(ctx, key).Bytes()
	if err != nil {
		return adjudicationVerdict{}, false
	}
	var verdict adjudicationVerdict
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return adjudicationVerdict{}, false
	}
	return verdict, true
}

func (s *Server) sharedSecretValueVerdictSet(host, value string, verdict adjudicationVerdict) {
	key := s.sharedSecretValueVerdictKey(host, value)
	if key == "" || s.redisClient == nil {
		return
	}
	raw, err := json.Marshal(verdict)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.redisClient.Set(ctx, key, raw, sharedRuntimeCacheTTL).Err()
}

func (s *Server) sharedCapturedSecretGet(key string) (capturedSecretEntry, bool) {
	if s == nil || s.redisClient == nil || key == "" {
		return capturedSecretEntry{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := s.redisClient.Get(ctx, redisCapturedSecretPrefix+key).Bytes()
	if err != nil {
		return capturedSecretEntry{}, false
	}
	var entry capturedSecretEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return capturedSecretEntry{}, false
	}
	return entry, entry.Placeholder != ""
}

func (s *Server) sharedCapturedSecretSet(key string, entry capturedSecretEntry) {
	if s == nil || s.redisClient == nil || key == "" {
		return
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.redisClient.Set(ctx, redisCapturedSecretPrefix+key, raw, sharedRuntimeCacheTTL).Err()
}

func (s *Server) acquireCapturedSecretLock(key string) (bool, func()) {
	if s == nil || s.redisClient == nil || key == "" {
		return false, func() {}
	}
	lockKey := redisCapturedSecretLockPrefix + key
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	token := time.Now().UTC().Format(time.RFC3339Nano)
	ok, err := s.redisClient.SetNX(ctx, lockKey, token, 15*time.Second).Result()
	if err != nil || !ok {
		return false, func() {}
	}
	return true, func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer releaseCancel()
		_ = s.redisClient.Del(releaseCtx, lockKey).Err()
	}
}

func (s *Server) sharedSecretValueVerdictKey(host, value string) string {
	if s == nil || s.redisClient == nil || host == "" || value == "" || len(s.secretCacheHMACKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, s.secretCacheHMACKey)
	mac.Write([]byte(host))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(value))
	return redisSecretValueVerdictPrefix + hex.EncodeToString(mac.Sum(nil))
}
