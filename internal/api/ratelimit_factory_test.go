package api

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/clawvisor/clawvisor/internal/ratelimit"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// TestNewKeyedLimiter_RedisBackedSharesCounterAcrossInstances proves the fix
// for audit finding #2a: when the server has a Redis client, the per-IP rate
// limiter it builds is backed by a counter shared across replicas. Two limiter
// instances (modeling two Cloud Run replicas) pointed at the same Redis enforce
// the configured cap in aggregate — not cap*N — so brute-force protection does
// not degrade as autoscaling adds replicas.
func TestNewKeyedLimiter_RedisBackedSharesCounterAcrossInstances(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// Two independent servers ("replicas") sharing one Redis, exactly as
	// defaults.go wires them from a single cfg.Redis.URL.
	replicaA := &Server{redisClient: rdb}
	replicaB := &Server{redisClient: rdb}

	bucket := config.RateLimitBucket{Limit: 10, Window: 60}
	limiterA := replicaA.newKeyedLimiterFromBucket(bucket)
	limiterB := replicaB.newKeyedLimiterFromBucket(bucket)

	if _, ok := limiterA.(*ratelimit.RedisKeyedLimiter); !ok {
		t.Fatalf("replica A limiter = %T, want *ratelimit.RedisKeyedLimiter", limiterA)
	}
	if _, ok := limiterB.(*ratelimit.RedisKeyedLimiter); !ok {
		t.Fatalf("replica B limiter = %T, want *ratelimit.RedisKeyedLimiter", limiterB)
	}

	const ip = "203.0.113.7"

	// Replica A serves 6 requests for this IP; all fit under the cap of 10.
	for i := 0; i < 6; i++ {
		if allowed, _, _ := limiterA.Allow(ip); !allowed {
			t.Fatalf("replica A request %d for %s denied, want allowed (cap 10)", i+1, ip)
		}
	}

	// Replica B, which never saw those requests locally, must observe the
	// remaining budget of 4 through the shared Redis counter.
	for i := 0; i < 4; i++ {
		if allowed, _, _ := limiterB.Allow(ip); !allowed {
			t.Fatalf("replica B request %d for %s denied, want allowed (4 left of cap 10)", i+1, ip)
		}
	}

	// The combined 11th request must be denied on either replica — the cap is
	// 10 across the fleet, not 10 per replica (which would be cap*N = 20).
	if allowed, _, _ := limiterB.Allow(ip); allowed {
		t.Fatalf("replica B allowed an 11th request for %s; cap of 10 not enforced across instances", ip)
	}
	if allowed, _, _ := limiterA.Allow(ip); allowed {
		t.Fatalf("replica A allowed an 11th request for %s; cap of 10 not enforced across instances", ip)
	}
}

// TestNewKeyedLimiter_InMemoryWhenNoRedis is the binding-invariant guard: a
// server with no Redis client (the single-VM / local-dev / self-hosted default)
// must keep using the in-memory per-process limiter, unchanged. This is what
// keeps the Redis-less single-instance deployment byte-for-byte as before.
func TestNewKeyedLimiter_InMemoryWhenNoRedis(t *testing.T) {
	s := &Server{} // redisClient nil, as when cfg.Redis.URL is empty

	limiter := s.newKeyedLimiterFromBucket(config.RateLimitBucket{Limit: 10, Window: 60})
	if _, ok := limiter.(*ratelimit.KeyedLimiter); !ok {
		t.Fatalf("no-Redis limiter = %T, want *ratelimit.KeyedLimiter (in-memory)", limiter)
	}

	// Unconfigured buckets still return nil regardless of the Redis path — the
	// caller relies on this to skip installing the rate-limit middleware.
	if got := s.newKeyedLimiterFromBucket(config.RateLimitBucket{Limit: 0, Window: 60}); got != nil {
		t.Fatalf("zero-limit bucket = %v, want nil", got)
	}
	if got := s.newKeyedLimiterFromBucket(config.RateLimitBucket{Limit: 10, Window: 0}); got != nil {
		t.Fatalf("zero-window bucket = %v, want nil", got)
	}
}
