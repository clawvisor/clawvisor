package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisCBTokenPrefix = "clawvisor:tgcb:"

// callbackEntryJSON is the JSON form for Redis storage.
type callbackEntryJSON struct {
	Type       string `json:"type"`
	TargetID   string `json:"target_id"`
	UserID     string `json:"user_id"`
	ChatID     string `json:"chat_id"`
	ExpiresAt  int64  `json:"expires_at"`
	SiblingID  string `json:"sibling_id"`
}

// redisCallbackTokenStore stores Telegram callback tokens in Redis for
// cross-instance access.
type redisCallbackTokenStore struct {
	rdb *redis.Client
}

// NewRedisCallbackTokenStore creates a Redis-backed callback token store.
func NewRedisCallbackTokenStore(rdb *redis.Client) CallbackTokenStorer {
	return &redisCallbackTokenStore{rdb: rdb}
}

func (s *redisCallbackTokenStore) Generate(entryType, targetID, userID, chatID string, ttl time.Duration) (string, string, error) {
	approveID, err := redisRandomShortID()
	if err != nil {
		return "", "", err
	}
	denyID, err := redisRandomShortID()
	if err != nil {
		return "", "", err
	}

	expiresAt := time.Now().Add(ttl).UnixMilli()

	approveData, err := json.Marshal(callbackEntryJSON{
		Type:      entryType,
		TargetID:  targetID,
		UserID:    userID,
		ChatID:    chatID,
		ExpiresAt: expiresAt,
		SiblingID: denyID,
	})
	if err != nil {
		return "", "", err
	}
	denyData, err := json.Marshal(callbackEntryJSON{
		Type:      entryType,
		TargetID:  targetID,
		UserID:    userID,
		ChatID:    chatID,
		ExpiresAt: expiresAt,
		SiblingID: approveID,
	})
	if err != nil {
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pipe := s.rdb.Pipeline()
	pipe.Set(ctx, redisCBTokenPrefix+approveID, approveData, ttl)
	pipe.Set(ctx, redisCBTokenPrefix+denyID, denyData, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return "", "", err
	}

	return approveID, denyID, nil
}

func (s *redisCallbackTokenStore) Consume(shortID string) (*callbackEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Atomically get and delete the consumed token.
	data, err := s.rdb.GetDel(ctx, redisCBTokenPrefix+shortID).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, errTokenNotFound
	}
	if err != nil {
		return nil, err
	}

	var j callbackEntryJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, errTokenNotFound
	}

	if time.Now().UnixMilli() > j.ExpiresAt {
		return nil, errTokenExpired
	}

	// Delete the sibling token so only one of approve/deny can succeed.
	if j.SiblingID != "" {
		s.rdb.Del(ctx, redisCBTokenPrefix+j.SiblingID)
	}

	return &callbackEntry{
		Type:      j.Type,
		TargetID:  j.TargetID,
		UserID:    j.UserID,
		ChatID:    j.ChatID,
		ExpiresAt: time.UnixMilli(j.ExpiresAt),
	}, nil
}

func (s *redisCallbackTokenStore) Cleanup() {} // Redis TTL handles expiry.

func redisRandomShortID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var _ CallbackTokenStorer = (*redisCallbackTokenStore)(nil)
