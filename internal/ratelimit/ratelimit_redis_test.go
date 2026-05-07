package ratelimit

import (
	"testing"
	"time"
)

// TestNormalizeRedisTTL is the regression guard against past-timestamp
// X-RateLimit-Reset values. Redis returns -1 / -2 for "no TTL" / "key
// missing" — we must NOT pass those through verbatim, otherwise resetTime
// = now + (-1s) = a past second, and clients see a meaningless header.
func TestNormalizeRedisTTL(t *testing.T) {
	const window = 60 * time.Second
	cases := []struct {
		name string
		raw  int64
		want time.Duration
	}{
		{"normal positive ttl", 42, 42 * time.Second},
		{"key has no expiry (-1) → fall back to window", -1, window},
		{"key missing (-2) → fall back to window", -2, window},
		{"zero ttl → fall back to window", 0, window},
		{"absurdly negative → fall back to window", -1000, window},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRedisTTL(tc.raw, window); got != tc.want {
				t.Fatalf("normalizeRedisTTL(%d, %s) = %s, want %s", tc.raw, window, got, tc.want)
			}
		})
	}
}
