package middleware

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisReplayCachePrefix = "clawvisor:replay:"

// RedisReplayCache stores HMAC nonces in Redis for cross-instance replay
// protection. Uses SET NX with TTL for atomic check-and-store.
type RedisReplayCache struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewRedisReplayCache creates a Redis-backed replay cache.
// TTL should match 2 * deviceTimestampSkew (10 minutes).
func NewRedisReplayCache(rdb *redis.Client) *RedisReplayCache {
	return &RedisReplayCache{rdb: rdb, ttl: 2 * deviceTimestampSkew}
}

func (c *RedisReplayCache) Check(key string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// SET NX returns false if the key already exists → replay detected.
	set, err := c.rdb.SetNX(ctx, redisReplayCachePrefix+key, "1", c.ttl).Result()
	if err != nil {
		// On Redis error, fall through (allow the request rather than blocking).
		return false
	}
	return !set // set==false means key existed → replay
}

var _ ReplayCache = (*RedisReplayCache)(nil)
var _ ReplayCache = memoryReplayCache{}
