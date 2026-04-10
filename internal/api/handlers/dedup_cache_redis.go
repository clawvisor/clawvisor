package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisDedupPrefix = "clawvisor:dedup:"

// redisDedupCache is a Redis-backed dedup cache for cross-instance
// request deduplication.
type redisDedupCache struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewRedisDedupCache creates a Redis-backed dedup cache.
func NewRedisDedupCache(rdb *redis.Client, ttl time.Duration) DedupCache {
	return &redisDedupCache{rdb: rdb, ttl: ttl}
}

func (c *redisDedupCache) Get(key dedupKey) (any, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := c.rdb.Get(ctx, redisDedupPrefix+string(key)).Bytes()
	if errors.Is(err, redis.Nil) || err != nil {
		return nil, false
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, false
	}
	return value, true
}

func (c *redisDedupCache) Put(key dedupKey, value any) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, redisDedupPrefix+string(key), data, c.ttl).Err()
}

func (c *redisDedupCache) Stop() {}
