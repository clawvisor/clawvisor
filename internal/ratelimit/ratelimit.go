package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// KeyedLimiter maintains per-key token bucket rate limiters.
type KeyedLimiter struct {
	r     rate.Limit
	burst int
	mu    sync.Mutex
	keys  map[string]*entry
	done  chan struct{}
}

// NewKeyedLimiter creates a limiter that allows r events per second with the
// given burst size. A background goroutine evicts stale entries every 5 minutes.
func NewKeyedLimiter(r rate.Limit, burst int) *KeyedLimiter {
	kl := &KeyedLimiter{
		r:     r,
		burst: burst,
		keys:  make(map[string]*entry),
		done:  make(chan struct{}),
	}
	go kl.cleanup()
	return kl
}

// Allow checks whether an event for the given key is allowed.
// Returns whether it was allowed, the approximate remaining burst tokens,
// and the time when the next token will be available.
func (kl *KeyedLimiter) Allow(key string) (allowed bool, remaining int, resetTime time.Time) {
	kl.mu.Lock()
	e, ok := kl.keys[key]
	if !ok {
		e = &entry{limiter: rate.NewLimiter(kl.r, kl.burst)}
		kl.keys[key] = e
	}
	e.lastSeen = time.Now()
	kl.mu.Unlock()

	now := time.Now()
	allowed = e.limiter.Allow()

	// Approximate remaining tokens (tokens are float; truncate to int).
	tokens := int(e.limiter.TokensAt(now))
	if tokens < 0 {
		tokens = 0
	}

	// Reset time: when the bucket will be full again.
	resetTime = now.Add(time.Duration(float64(kl.burst-tokens) / float64(kl.r) * float64(time.Second)))

	return allowed, tokens, resetTime
}

// Stop terminates the background cleanup goroutine.
func (kl *KeyedLimiter) Stop() {
	close(kl.done)
}

func (kl *KeyedLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-kl.done:
			return
		case <-ticker.C:
			kl.mu.Lock()
			cutoff := time.Now().Add(-10 * time.Minute)
			for k, e := range kl.keys {
				if e.lastSeen.Before(cutoff) {
					delete(kl.keys, k)
				}
			}
			kl.mu.Unlock()
		}
	}
}
