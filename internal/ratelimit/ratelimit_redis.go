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
	return rl.AllowN(key, 1)
}

// AllowN atomically attempts to take n tokens. Either all n are consumed
// or none — see KeyedLimiter.AllowN. The Lua script reads-checks-INCRBY in
// one atomic Redis operation so a partial charge is impossible even under
// concurrent callers.
func (rl *RedisKeyedLimiter) AllowN(key string, n int) (allowed bool, remaining int, resetTime time.Time) {
	if n < 1 {
		n = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rkey := redisRateLimitPrefix + key

	// Read current count; if count+n would exceed burst, take nothing and
	// return denial. Otherwise INCRBY n in the same atomic script. If the
	// key is fresh, set the window TTL.
	script := redis.NewScript(`
		local current = tonumber(redis.call('GET', KEYS[1]) or '0')
		local n = tonumber(ARGV[2])
		local burst = tonumber(ARGV[3])
		if current + n > burst then
			return {current, redis.call('TTL', KEYS[1]), 0}
		end
		local newCount = redis.call('INCRBY', KEYS[1], n)
		if current == 0 then
			redis.call('EXPIRE', KEYS[1], ARGV[1])
		end
		return {newCount, redis.call('TTL', KEYS[1]), 1}
	`)

	result, err := script.Run(ctx, rl.rdb,
		[]string{rkey},
		int(rl.window.Seconds()), n, rl.burst,
	).Int64Slice()
	if err != nil {
		// On Redis error, allow the request (fail-open) — same posture as
		// the single-token path.
		return true, rl.burst, time.Now().Add(rl.window)
	}

	count := int(result[0])
	allowed = result[2] == 1
	remaining = rl.burst - count
	if remaining < 0 {
		remaining = 0
	}
	resetTime = time.Now().Add(normalizeRedisTTL(result[1], rl.window))
	return
}

// normalizeRedisTTL converts a raw Redis TTL response into a sensible
// duration relative to the rate-limit window.
//
// Redis TTL semantics:
//   - positive N: N whole seconds remaining (rounded down).
//   - 0:          key expires within the current second — a valid value.
//   - -1:         key exists but has no expiry set.
//   - -2:         key does not exist.
//
// Only the two negative values are sentinels; both can surface on the
// deny path (key set without EXPIRE by some other producer, or key just
// expired between our GET and the TTL call). For those, fall back to the
// configured window so X-RateLimit-Reset isn't a past timestamp. TTL=0
// is preserved verbatim — overriding it would overstate the reset time
// by a full window for keys that are about to refill.
func normalizeRedisTTL(ttlSecs int64, window time.Duration) time.Duration {
	if ttlSecs >= 0 {
		return time.Duration(ttlSecs) * time.Second
	}
	return window
}

// Stop is a no-op for Redis-backed limiter.
func (rl *RedisKeyedLimiter) Stop() {}

// Limiter is the interface satisfied by both KeyedLimiter and RedisKeyedLimiter.
type Limiter interface {
	Allow(key string) (allowed bool, remaining int, resetTime time.Time)
	// AllowN atomically takes n tokens. Either n are consumed or none —
	// the limiter must not partially charge on denial. Required by
	// callers (e.g. /api/gateway/batch) that fan out N sub-requests per
	// HTTP call and need to know up-front whether the whole fan-out fits.
	AllowN(key string, n int) (allowed bool, remaining int, resetTime time.Time)
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
