package handlers

import (
	"sync"
	"time"
)

// ClaimCodeCache stores short-lived single-use claim codes that authorize
// a bootstrap curl to be attributed to a specific user without exposing the
// user's ID in the URL. Codes are minted by an authenticated session and
// consumed (atomically) by the unauthenticated POST /api/agents/connect
// endpoint when the curl runs.
type ClaimCodeCache interface {
	// Store records a claim code for the user with the given TTL.
	// Returns an error when the underlying backend can't persist the
	// entry; callers should refuse to hand the code back to the user.
	Store(code, userID string, ttl time.Duration) error
	// Peek returns the user ID for a claim without consuming it. Lets
	// callers validate the request before burning the single-use code, so
	// recoverable failures (duplicate-name 409, max-pending 429) don't
	// strand the dashboard with a stale claim it can't refresh for
	// minutes.
	Peek(code string) (userID string, ok bool)
	// Consume atomically validates+removes the claim code. Returns the
	// user ID if the code is valid and unused; the second value is false
	// for unknown, expired, or already-consumed codes.
	Consume(code string) (userID string, ok bool)
}

type claimCodeEntry struct {
	userID    string
	expiresAt time.Time
}

type memoryClaimCodeCache struct {
	mu      sync.Mutex
	entries map[string]claimCodeEntry
}

func newMemoryClaimCodeCache() *memoryClaimCodeCache {
	return &memoryClaimCodeCache{entries: make(map[string]claimCodeEntry)}
}

func (c *memoryClaimCodeCache) Store(code, userID string, ttl time.Duration) error {
	c.mu.Lock()
	c.entries[code] = claimCodeEntry{userID: userID, expiresAt: time.Now().Add(ttl)}
	// Opportunistic cleanup piggy-backed on writes, while we already hold
	// the lock. Avoids the per-Store goroutine that the original code
	// fired (each call spawned a fresh goroutine all contending for this
	// same mutex, stacking up under load).
	c.cleanupLocked()
	c.mu.Unlock()
	return nil
}

func (c *memoryClaimCodeCache) Peek(code string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[code]
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, code)
		return "", false
	}
	return entry.userID, true
}

func (c *memoryClaimCodeCache) Consume(code string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[code]
	if !ok {
		return "", false
	}
	// Always remove on lookup — single-use, even if expired.
	delete(c.entries, code)
	if time.Now().After(entry.expiresAt) {
		return "", false
	}
	return entry.userID, true
}

// cleanupLocked is the actual eviction loop; the caller must already hold
// c.mu. Inlined into Store so we don't pay a per-Store goroutine.
func (c *memoryClaimCodeCache) cleanupLocked() {
	now := time.Now()
	for code, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, code)
		}
	}
}
