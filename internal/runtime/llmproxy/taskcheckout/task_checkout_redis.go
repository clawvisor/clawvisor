package taskcheckout

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

type RedisStore struct {
	rdb        *redis.Client
	defaultTTL time.Duration
	now        func() time.Time
}

func NewRedisStore(rdb *redis.Client, defaultTTL time.Duration) *RedisStore {
	if defaultTTL <= 0 {
		defaultTTL = 24 * time.Hour
	}
	return &RedisStore{rdb: rdb, defaultTTL: defaultTTL, now: time.Now}
}

func (s *RedisStore) Set(ctx context.Context, key Key, taskID string, ttl time.Duration) error {
	if s == nil || s.rdb == nil || key.UserID == "" || key.AgentID == "" || taskID == "" {
		return nil
	}
	if key.ConversationID == "" {
		return ErrConversationIDRequired
	}
	if ttl <= 0 {
		ttl = s.defaultTTL
	}
	now := s.now().UTC()
	checkout := Checkout{
		TaskID:    taskID,
		UpdatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	raw, err := json.Marshal(checkout)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, redisKey(key), raw, ttl).Err()
}

func (s *RedisStore) Get(ctx context.Context, key Key) (Checkout, bool, error) {
	if s == nil || s.rdb == nil || key.UserID == "" || key.AgentID == "" || key.ConversationID == "" {
		return Checkout{}, false, nil
	}
	raw, err := s.rdb.Get(ctx, redisKey(key)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return Checkout{}, false, nil
		}
		return Checkout{}, false, err
	}
	var checkout Checkout
	if err := json.Unmarshal(raw, &checkout); err != nil {
		return Checkout{}, false, err
	}
	if !checkout.ExpiresAt.IsZero() && s.now().UTC().After(checkout.ExpiresAt) {
		_ = s.rdb.Del(ctx, redisKey(key)).Err()
		return Checkout{}, false, nil
	}
	return checkout, true, nil
}

func (s *RedisStore) Clear(ctx context.Context, key Key) error {
	if s == nil || s.rdb == nil || key.UserID == "" || key.AgentID == "" || key.ConversationID == "" {
		return nil
	}
	return s.rdb.Del(ctx, redisKey(key)).Err()
}

func redisKey(key Key) string {
	// ConversationID is required at the Store layer (Set returns
	// ErrConversationIDRequired and Get returns not-found when it's
	// empty), so we only ever reach this function with a non-empty
	// ConversationID. Hashing it into the key partitions focus
	// per-conversation, which is the invariant that prevents one
	// conversation's checkout from authorizing another conversation's
	// tool calls.
	sum := sha256.Sum256([]byte(key.UserID + "\x00" + key.AgentID + "\x00" + key.ConversationID))
	return redisTaskCheckoutPrefix + hex.EncodeToString(sum[:])
}

var _ Store = (*RedisStore)(nil)
