package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisClaimCachePrefix = "clawvisor:claimcode:"

// RedisClaimCodeCache stores short-lived single-use claim codes in Redis so
// the bootstrap-curl POST can land on any instance and still consume a code
// minted on another. Single-use is enforced atomically via GETDEL.
type RedisClaimCodeCache struct {
	rdb *redis.Client
}

// NewRedisClaimCodeCache creates a Redis-backed claim code cache. TTL is
// passed per-Store call so the in-memory and Redis variants have the same
// shape; the caller (the connections handler) decides the TTL.
func NewRedisClaimCodeCache(rdb *redis.Client) *RedisClaimCodeCache {
	return &RedisClaimCodeCache{rdb: rdb}
}

func (c *RedisClaimCodeCache) Store(code, userID string, ttl time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.rdb.Set(ctx, redisClaimCachePrefix+code, userID, ttl).Err()
}

// Peek reads the user ID without deleting the entry. Lets the connections
// handler validate the request shape before burning the claim — failures
// that the user can recover from (duplicate name, max pending) shouldn't
// burn their single-use code.
func (c *RedisClaimCodeCache) Peek(code string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	val, err := c.rdb.Get(ctx, redisClaimCachePrefix+code).Result()
	if errors.Is(err, redis.Nil) || err != nil {
		return "", false
	}
	return val, true
}

// Consume reads and deletes the entry in one round-trip via GETDEL, so two
// concurrent consumes of the same code can't both succeed. Returns ("",
// false) for unknown, expired (Redis already evicted), or already-consumed
// codes.
func (c *RedisClaimCodeCache) Consume(code string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	val, err := c.rdb.GetDel(ctx, redisClaimCachePrefix+code).Result()
	if errors.Is(err, redis.Nil) || err != nil {
		return "", false
	}
	return val, true
}
