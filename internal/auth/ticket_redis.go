package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisTicketPrefix = "clawvisor:ticket:"

// RedisTicketStore implements TicketStorer using Redis.
// Token expiry is handled by Redis TTL; Cleanup is a no-op.
type RedisTicketStore struct {
	rdb *redis.Client
}

// NewRedisTicketStore creates a Redis-backed ticket store.
func NewRedisTicketStore(rdb *redis.Client) *RedisTicketStore {
	return &RedisTicketStore{rdb: rdb}
}

// Generate creates a new SSE ticket for the given user, stored in Redis
// with a 30-second TTL.
func (s *RedisTicketStore) Generate(userID string) (string, error) {
	b := make([]byte, ticketBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	ticket := hex.EncodeToString(b)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.rdb.Set(ctx, redisTicketPrefix+ticket, userID, ticketExpiry).Err(); err != nil {
		return "", err
	}
	return ticket, nil
}

// Validate atomically reads and deletes a ticket (single-use).
func (s *RedisTicketStore) Validate(ticket string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	userID, err := s.rdb.GetDel(ctx, redisTicketPrefix+ticket).Result()
	if errors.Is(err, redis.Nil) {
		return "", errors.New("ticket: not found or expired")
	}
	if err != nil {
		return "", err
	}
	return userID, nil
}

// Cleanup is a no-op — Redis TTL handles expiry automatically.
func (s *RedisTicketStore) Cleanup() {}
