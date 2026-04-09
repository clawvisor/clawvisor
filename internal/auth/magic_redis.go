package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisMagicPrefix = "clawvisor:magic:"

// RedisMagicTokenStore implements pkg/auth.MagicTokenStore using Redis.
// Token expiry is handled by Redis TTL; Cleanup is a no-op.
type RedisMagicTokenStore struct {
	rdb *redis.Client
}

// NewRedisMagicTokenStore creates a Redis-backed magic token store.
func NewRedisMagicTokenStore(rdb *redis.Client) *RedisMagicTokenStore {
	return &RedisMagicTokenStore{rdb: rdb}
}

// Generate creates a new magic token for the given user, stored in Redis
// with a 15-minute TTL.
func (s *RedisMagicTokenStore) Generate(userID string) (string, error) {
	b := make([]byte, magicTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.rdb.Set(ctx, redisMagicPrefix+token, userID, magicTokenExpiry).Err(); err != nil {
		return "", err
	}
	return token, nil
}

// Validate atomically reads and deletes a magic token (single-use).
// Requires Redis 6.2+ for GETDEL.
func (s *RedisMagicTokenStore) Validate(token string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	userID, err := s.rdb.GetDel(ctx, redisMagicPrefix+token).Result()
	if errors.Is(err, redis.Nil) {
		return "", errors.New("magic: token not found or expired")
	}
	if err != nil {
		return "", err
	}
	return userID, nil
}

// Cleanup is a no-op — Redis TTL handles expiry automatically.
func (s *RedisMagicTokenStore) Cleanup() {}
