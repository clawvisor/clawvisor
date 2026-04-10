package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisOAuthStatePrefix  = "clawvisor:oauth:"
	redisDeviceFlowPrefix  = "clawvisor:deviceflow:"
	redisPKCEFlowPrefix    = "clawvisor:pkceflow:"
	defaultOAuthStateTTL   = 15 * time.Minute
)

// RedisOAuthStateStore stores OAuth flow state in Redis for cross-instance access.
type RedisOAuthStateStore struct {
	rdb *redis.Client
}

// NewRedisOAuthStateStore creates a Redis-backed OAuth state store.
func NewRedisOAuthStateStore(rdb *redis.Client) *RedisOAuthStateStore {
	return &RedisOAuthStateStore{rdb: rdb}
}

func (s *RedisOAuthStateStore) StoreOAuth(state string, entry oauthStateEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := marshalOAuthState(entry)
	if err != nil {
		return
	}
	ttl := time.Until(entry.ExpiresAt)
	if ttl <= 0 {
		ttl = defaultOAuthStateTTL
	}
	_ = s.rdb.Set(ctx, redisOAuthStatePrefix+state, data, ttl).Err()
}

func (s *RedisOAuthStateStore) LoadAndDeleteOAuth(state string) (oauthStateEntry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := s.rdb.GetDel(ctx, redisOAuthStatePrefix+state).Bytes()
	if errors.Is(err, redis.Nil) || err != nil {
		return oauthStateEntry{}, false
	}
	entry, err := unmarshalOAuthState(data)
	if err != nil {
		return oauthStateEntry{}, false
	}
	return entry, true
}

func (s *RedisOAuthStateStore) StoreDeviceFlow(flowID string, entry deviceFlowEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	ttl := time.Until(entry.ExpiresAt)
	if ttl <= 0 {
		ttl = defaultOAuthStateTTL
	}
	_ = s.rdb.Set(ctx, redisDeviceFlowPrefix+flowID, data, ttl).Err()
}

func (s *RedisOAuthStateStore) LoadDeviceFlow(flowID string) (deviceFlowEntry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := s.rdb.Get(ctx, redisDeviceFlowPrefix+flowID).Bytes()
	if errors.Is(err, redis.Nil) || err != nil {
		return deviceFlowEntry{}, false
	}
	var entry deviceFlowEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return deviceFlowEntry{}, false
	}
	return entry, true
}

func (s *RedisOAuthStateStore) UpdateDeviceFlow(flowID string, entry deviceFlowEntry) {
	s.StoreDeviceFlow(flowID, entry)
}

func (s *RedisOAuthStateStore) DeleteDeviceFlow(flowID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.rdb.Del(ctx, redisDeviceFlowPrefix+flowID).Err()
}

func (s *RedisOAuthStateStore) StorePKCE(state string, entry pkceFlowEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	ttl := time.Until(entry.ExpiresAt)
	if ttl <= 0 {
		ttl = defaultOAuthStateTTL
	}
	_ = s.rdb.Set(ctx, redisPKCEFlowPrefix+state, data, ttl).Err()
}

func (s *RedisOAuthStateStore) LoadAndDeletePKCE(state string) (pkceFlowEntry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := s.rdb.GetDel(ctx, redisPKCEFlowPrefix+state).Bytes()
	if errors.Is(err, redis.Nil) || err != nil {
		return pkceFlowEntry{}, false
	}
	var entry pkceFlowEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return pkceFlowEntry{}, false
	}
	return entry, true
}

// Cleanup is a no-op — Redis TTL handles expiry automatically.
func (s *RedisOAuthStateStore) Cleanup() {}

var _ OAuthStateStore = (*RedisOAuthStateStore)(nil)
