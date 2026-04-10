package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisTokenCachePrefix = "clawvisor:conntoken:"

// RedisTokenCache stores approved agent tokens in Redis so any instance
// can serve the PollStatus request after a different instance approved.
type RedisTokenCache struct {
	rdb    *redis.Client
	window time.Duration
}

// NewRedisTokenCache creates a Redis-backed token cache.
func NewRedisTokenCache(rdb *redis.Client, window time.Duration) *RedisTokenCache {
	return &RedisTokenCache{rdb: rdb, window: window}
}

func (c *RedisTokenCache) Store(id string, rawToken string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.rdb.Set(ctx, redisTokenCachePrefix+id, rawToken, c.window).Err()
}

func (c *RedisTokenCache) Load(id string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	val, err := c.rdb.Get(ctx, redisTokenCachePrefix+id).Result()
	if errors.Is(err, redis.Nil) || err != nil {
		return "", false
	}
	return val, true
}
