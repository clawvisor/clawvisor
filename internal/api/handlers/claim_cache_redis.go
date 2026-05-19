package handlers

import (
	"context"
	"errors"
	"log/slog"
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

// Store persists the code:userID mapping with the given TTL. Errors here
// must be surfaced — if the SET silently failed, the dashboard would hand
// the user a 201 with a code that doesn't exist in Redis, and the next
// bootstrap curl would immediately INVALID_CLAIM.
func (c *RedisClaimCodeCache) Store(code, userID string, ttl time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.rdb.Set(ctx, redisClaimCachePrefix+code, userID, ttl).Err()
}

// Peek reads the user ID without deleting the entry. Lets the connections
// handler validate the request shape before burning the claim. A transient
// Redis error is reported to the caller as a miss (the current interface
// can't distinguish), but is logged so the symptom (user-facing
// INVALID_CLAIM) is at least diagnosable from the daemon side.
func (c *RedisClaimCodeCache) Peek(code string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	val, err := c.rdb.Get(ctx, redisClaimCachePrefix+code).Result()
	if errors.Is(err, redis.Nil) {
		return "", false
	}
	if err != nil {
		slog.WarnContext(ctx, "redis claim cache: Peek failed (treating as miss)",
			"err", err.Error())
		return "", false
	}
	return val, true
}

// Consume reads and deletes the entry in one round-trip via GETDEL, so two
// concurrent consumes of the same code can't both succeed. Returns ("",
// false) for unknown, expired (Redis already evicted), or already-consumed
// codes; transient Redis errors are logged separately from legitimate
// misses.
func (c *RedisClaimCodeCache) Consume(code string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	val, err := c.rdb.GetDel(ctx, redisClaimCachePrefix+code).Result()
	if errors.Is(err, redis.Nil) {
		return "", false
	}
	if err != nil {
		slog.WarnContext(ctx, "redis claim cache: Consume failed (treating as miss)",
			"err", err.Error())
		return "", false
	}
	return val, true
}
