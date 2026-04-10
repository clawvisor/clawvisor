package intent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisVerdictPrefix = "clawvisor:verdict:"

// redisVerdictCache stores intent verification verdicts in Redis for
// cross-instance sharing, avoiding redundant LLM calls.
type redisVerdictCache struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewRedisVerdictCache creates a Redis-backed verdict cache.
func NewRedisVerdictCache(rdb *redis.Client, ttl time.Duration) VerdictCacher {
	return &redisVerdictCache{rdb: rdb, ttl: ttl}
}

func (c *redisVerdictCache) Get(key cacheKey) (*VerificationVerdict, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := c.rdb.Get(ctx, redisVerdictPrefix+string(key)).Bytes()
	if errors.Is(err, redis.Nil) || err != nil {
		return nil, false
	}
	var verdict VerificationVerdict
	if err := json.Unmarshal(data, &verdict); err != nil {
		return nil, false
	}
	// Return a copy so callers can mutate (e.g. set Cached=true).
	cp := verdict
	return &cp, true
}

func (c *redisVerdictCache) Put(key cacheKey, verdict *VerificationVerdict) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := json.Marshal(verdict)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, redisVerdictPrefix+string(key), data, c.ttl).Err()
}

func (c *redisVerdictCache) Cleanup() {} // Redis TTL handles expiry.
