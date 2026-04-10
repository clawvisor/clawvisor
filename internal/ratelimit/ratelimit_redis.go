package ratelimit

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisRateLimitPrefix = "clawvisor:rl:"

// RedisKeyedLimiter implements a sliding-window rate limiter using Redis.
// Each key maps to a Redis key with an expiring counter. This ensures rate
// limits are enforced across all server instances.
type RedisKeyedLimiter struct {
	rdb    *redis.Client
	r      float64 // rate (events/second)
	burst  int
	window time.Duration
}

// NewRedisKeyedLimiter creates a Redis-backed rate limiter.
func NewRedisKeyedLimiter(rdb *redis.Client, r float64, burst int, window time.Duration) *RedisKeyedLimiter {
	return &RedisKeyedLimiter{rdb: rdb, r: r, burst: burst, window: window}
}

// Allow checks whether an event for the given key is allowed using a
// fixed-window counter in Redis. Returns the same signature as KeyedLimiter
// for drop-in compatibility.
func (rl *RedisKeyedLimiter) Allow(key string) (allowed bool, remaining int, resetTime time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rkey := redisRateLimitPrefix + key

	// Lua script: INCR + conditional EXPIRE, returns [count, ttl].
	script := redis.NewScript(`
		local count = redis.call('INCR', KEYS[1])
		if count == 1 then
			redis.call('EXPIRE', KEYS[1], ARGV[1])
		end
		local ttl = redis.call('TTL', KEYS[1])
		return {count, ttl}
	`)

	result, err := script.Run(ctx, rl.rdb, []string{rkey}, int(rl.window.Seconds())).Int64Slice()
	if err != nil {
		// On Redis error, allow the request (fail-open).
		return true, rl.burst, time.Now().Add(rl.window)
	}

	count := int(result[0])
	ttl := time.Duration(result[1]) * time.Second

	allowed = count <= rl.burst
	remaining = rl.burst - count
	if remaining < 0 {
		remaining = 0
	}
	resetTime = time.Now().Add(ttl)
	return
}

// Stop is a no-op for Redis-backed limiter.
func (rl *RedisKeyedLimiter) Stop() {}

// Limiter is the interface satisfied by both KeyedLimiter and RedisKeyedLimiter.
type Limiter interface {
	Allow(key string) (allowed bool, remaining int, resetTime time.Time)
	Stop()
}

// Compile-time interface verification.
var _ Limiter = (*KeyedLimiter)(nil)
var _ Limiter = (*RedisKeyedLimiter)(nil)

// Conversion helper for config. Called when building the rate limiter for
// use in NewRedisKeyedLimiter.
func WindowFromRateAndBurst(r float64, burst int) time.Duration {
	if r <= 0 {
		return time.Minute
	}
	return time.Duration(float64(burst)/r) * time.Second
}

// TokensString is used for debugging / header output.
func TokensString(remaining int) string {
	return strconv.Itoa(remaining)
}
