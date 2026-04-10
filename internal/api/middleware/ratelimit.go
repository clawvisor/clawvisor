package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/clawvisor/clawvisor/internal/ratelimit"
)

// RateLimit returns middleware that enforces per-key rate limiting using the
// provided KeyedLimiter. keyFunc extracts the rate-limit key from the request
// (e.g. agent ID or user ID from context). If keyFunc returns "", the request
// is not rate-limited (unauthenticated). A nil limiter disables rate limiting.
func RateLimit(limiter ratelimit.Limiter, keyFunc func(*http.Request) string, limit int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil {
				next.ServeHTTP(w, r)
				return
			}
			key := keyFunc(r)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			allowed, remaining, resetTime := limiter.Allow(key)

			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetTime.Unix(), 10))

			if !allowed {
				retryAfter := time.Until(resetTime).Seconds()
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", fmt.Sprintf("%.0f", retryAfter))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "rate limit exceeded",
					"code":  "RATE_LIMITED",
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
