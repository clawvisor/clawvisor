package llmproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisTaskCheckoutPrefix = "clawvisor:lite_task_checkout:"

type RedisTaskCheckoutStore struct {
	rdb        *redis.Client
	defaultTTL time.Duration
	now        func() time.Time
}

func NewRedisTaskCheckoutStore(rdb *redis.Client, defaultTTL time.Duration) *RedisTaskCheckoutStore {
	if defaultTTL <= 0 {
		defaultTTL = 24 * time.Hour
	}
	return &RedisTaskCheckoutStore{rdb: rdb, defaultTTL: defaultTTL, now: time.Now}
}

func (s *RedisTaskCheckoutStore) Set(ctx context.Context, key TaskCheckoutKey, taskID string, ttl time.Duration) error {
	if s == nil || s.rdb == nil || key.UserID == "" || key.AgentID == "" || taskID == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = s.defaultTTL
	}
	now := s.now().UTC()
	checkout := TaskCheckout{
		TaskID:    taskID,
		UpdatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	raw, err := json.Marshal(checkout)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, redisTaskCheckoutKey(key), raw, ttl).Err()
}

func (s *RedisTaskCheckoutStore) Get(ctx context.Context, key TaskCheckoutKey) (TaskCheckout, bool, error) {
	if s == nil || s.rdb == nil || key.UserID == "" || key.AgentID == "" {
		return TaskCheckout{}, false, nil
	}
	raw, err := s.rdb.Get(ctx, redisTaskCheckoutKey(key)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return TaskCheckout{}, false, nil
		}
		return TaskCheckout{}, false, err
	}
	var checkout TaskCheckout
	if err := json.Unmarshal(raw, &checkout); err != nil {
		return TaskCheckout{}, false, err
	}
	if !checkout.ExpiresAt.IsZero() && s.now().UTC().After(checkout.ExpiresAt) {
		_ = s.rdb.Del(ctx, redisTaskCheckoutKey(key)).Err()
		return TaskCheckout{}, false, nil
	}
	return checkout, true, nil
}

func (s *RedisTaskCheckoutStore) Clear(ctx context.Context, key TaskCheckoutKey) error {
	if s == nil || s.rdb == nil || key.UserID == "" || key.AgentID == "" {
		return nil
	}
	return s.rdb.Del(ctx, redisTaskCheckoutKey(key)).Err()
}

func redisTaskCheckoutKey(key TaskCheckoutKey) string {
	// ConversationID partitions focus per-conversation. When empty, we
	// hash the legacy (user, agent) shape unchanged so existing redis
	// entries from pre-conversation-scoping clients remain readable —
	// no migration required, no silent loss of focus on upgrade.
	if key.ConversationID == "" {
		sum := sha256.Sum256([]byte(key.UserID + "\x00" + key.AgentID))
		return redisTaskCheckoutPrefix + hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256([]byte(key.UserID + "\x00" + key.AgentID + "\x00" + key.ConversationID))
	return redisTaskCheckoutPrefix + hex.EncodeToString(sum[:])
}

var _ TaskCheckoutStore = (*RedisTaskCheckoutStore)(nil)
