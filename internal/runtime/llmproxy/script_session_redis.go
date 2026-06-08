package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisScriptSessionPrefix = "clawvisor:script_session:"

// RedisScriptSessionCache stores script sessions in Redis so a session minted
// by one proxy instance can be authorized and accounted by another. Mutating
// operations use WATCH transactions so validation and counter updates remain
// atomic under concurrent resolver requests.
type RedisScriptSessionCache struct {
	rdb                     *redis.Client
	now                     func() time.Time
	beforeRecordBytesCommit func(token string)
}

// NewRedisScriptSessionCache returns a Redis-backed script-session cache.
func NewRedisScriptSessionCache(rdb *redis.Client) *RedisScriptSessionCache {
	return &RedisScriptSessionCache{rdb: rdb, now: time.Now}
}

type redisScriptSessionPayload struct {
	Session ScriptSession `json:"session"`
}

func redisScriptSessionKey(token string) string {
	return redisScriptSessionPrefix + token
}

func (c *RedisScriptSessionCache) ttl(sess ScriptSession) time.Duration {
	if sess.ExpiresAt.IsZero() {
		return 0
	}
	return sess.ExpiresAt.Sub(c.now())
}

// Mint implements ScriptSessionCache.
func (c *RedisScriptSessionCache) Mint(ctx context.Context, sess ScriptSession) (string, error) {
	token, err := generateScriptSessionToken()
	if err != nil {
		return "", err
	}
	sess = normalizeScriptSession(sess)
	raw, err := json.Marshal(redisScriptSessionPayload{Session: sess})
	if err != nil {
		return "", err
	}
	ttl := c.ttl(sess)
	if !sess.ExpiresAt.IsZero() && ttl <= 0 {
		ttl = time.Nanosecond
	}
	if err := c.rdb.Set(ctx, redisScriptSessionKey(token), raw, ttl).Err(); err != nil {
		return "", err
	}
	return token, nil
}

// Authorize implements ScriptSessionCache.
func (c *RedisScriptSessionCache) Authorize(ctx context.Context, token string, req ScriptSessionRequest) (ScriptSession, error) {
	req = normalizeScriptSessionRequest(req)
	key := redisScriptSessionKey(token)
	var out ScriptSession
	for {
		err := c.rdb.Watch(ctx, func(tx *redis.Tx) error {
			sess, err := c.getSessionForUpdate(ctx, tx, key)
			if err != nil {
				return err
			}
			if sess.TargetHost != req.Host {
				return &ScopeMismatchDetail{Field: "host", Got: req.Host, Expected: []string{sess.TargetHost}}
			}
			if !sess.methodAllowed(req.Method) {
				return &ScopeMismatchDetail{Field: "method", Got: req.Method, Expected: append([]string{}, sess.Methods...)}
			}
			if !sess.pathAllowed(req.Path) {
				return &ScopeMismatchDetail{Field: "path", Got: req.Path, Expected: append([]string{}, sess.PathPrefixes...)}
			}
			if req.Placeholder == "" || req.Placeholder != sess.Placeholder {
				return &ScopeMismatchDetail{Field: "placeholder", Got: req.Placeholder, Expected: []string{sess.Placeholder}}
			}
			if sess.MaxUses > 0 && sess.UsedCount >= sess.MaxUses {
				return ErrScriptSessionExhausted
			}
			if sess.MaxTotalBytes > 0 && sess.TotalBytesUsed >= sess.MaxTotalBytes {
				return ErrScriptSessionBytesExceeded
			}
			if sess.MaxRequestBytes > 0 && sess.MaxTotalBytes > 0 {
				if sess.TotalBytesUsed+sess.MaxRequestBytes > sess.MaxTotalBytes {
					return ErrScriptSessionBytesExceeded
				}
				sess.TotalBytesUsed += sess.MaxRequestBytes
			}
			sess.UsedCount++
			out = sess
			return c.setSessionTx(ctx, tx, key, sess)
		}, key)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		return out, err
	}
}

// RecordBytes implements ScriptSessionCache.
func (c *RedisScriptSessionCache) RecordBytes(ctx context.Context, token string, bytes int64) (ScriptSession, error) {
	key := redisScriptSessionKey(token)
	var out ScriptSession
	for {
		var exceeded bool
		err := c.rdb.Watch(ctx, func(tx *redis.Tx) error {
			sess, err := c.getSessionForUpdate(ctx, tx, key)
			if err != nil {
				if errors.Is(err, ErrScriptSessionNotFound) || errors.Is(err, ErrScriptSessionExpired) {
					out = ScriptSession{}
					return nil
				}
				return err
			}
			switch {
			case sess.MaxRequestBytes > 0 && sess.MaxTotalBytes > 0:
				overReservation := sess.MaxRequestBytes - bytes
				sess.TotalBytesUsed -= overReservation
				if sess.TotalBytesUsed < 0 {
					sess.TotalBytesUsed = 0
				}
			case bytes > 0:
				sess.TotalBytesUsed += bytes
			}
			if sess.MaxTotalBytes > 0 && sess.TotalBytesUsed > sess.MaxTotalBytes {
				exceeded = true
			}
			out = sess
			if c.beforeRecordBytesCommit != nil {
				c.beforeRecordBytesCommit(token)
			}
			if err := c.setSessionTx(ctx, tx, key, sess); err != nil {
				return err
			}
			if exceeded {
				return ErrScriptSessionBytesExceeded
			}
			return nil
		}, key)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		return out, err
	}
}

// ReleaseAuthorize implements ScriptSessionCache.
func (c *RedisScriptSessionCache) ReleaseAuthorize(ctx context.Context, token string) error {
	key := redisScriptSessionKey(token)
	for {
		err := c.rdb.Watch(ctx, func(tx *redis.Tx) error {
			sess, err := c.getSessionForUpdate(ctx, tx, key)
			if err != nil {
				if errors.Is(err, ErrScriptSessionNotFound) || errors.Is(err, ErrScriptSessionExpired) {
					return nil
				}
				return err
			}
			if sess.MaxRequestBytes > 0 && sess.MaxTotalBytes > 0 {
				sess.TotalBytesUsed -= sess.MaxRequestBytes
				if sess.TotalBytesUsed < 0 {
					sess.TotalBytesUsed = 0
				}
			}
			if sess.UsedCount > 0 {
				sess.UsedCount--
			}
			return c.setSessionTx(ctx, tx, key, sess)
		}, key)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		return err
	}
}

// Revoke implements ScriptSessionCache.
func (c *RedisScriptSessionCache) Revoke(ctx context.Context, token string) error {
	return c.rdb.Del(ctx, redisScriptSessionKey(token)).Err()
}

func (c *RedisScriptSessionCache) getSessionForUpdate(ctx context.Context, tx *redis.Tx, key string) (ScriptSession, error) {
	raw, err := tx.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ScriptSession{}, ErrScriptSessionNotFound
		}
		return ScriptSession{}, err
	}
	var payload redisScriptSessionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		_, _ = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, key)
			return nil
		})
		return ScriptSession{}, ErrScriptSessionNotFound
	}
	sess := normalizeScriptSession(payload.Session)
	if !sess.ExpiresAt.IsZero() && c.now().After(sess.ExpiresAt) {
		_, _ = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, key)
			return nil
		})
		return ScriptSession{}, ErrScriptSessionExpired
	}
	return sess, nil
}

func (c *RedisScriptSessionCache) setSessionTx(ctx context.Context, tx *redis.Tx, key string, sess ScriptSession) error {
	raw, err := json.Marshal(redisScriptSessionPayload{Session: sess})
	if err != nil {
		return err
	}
	ttl := c.ttl(sess)
	if !sess.ExpiresAt.IsZero() && ttl <= 0 {
		_, _ = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, key)
			return nil
		})
		return ErrScriptSessionExpired
	}
	_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, key, raw, ttl)
		return nil
	})
	return err
}

var _ ScriptSessionCache = (*RedisScriptSessionCache)(nil)
