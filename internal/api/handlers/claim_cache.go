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
	Store(code, userID string, ttl time.Duration)
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

func (c *memoryClaimCodeCache) Store(code, userID string, ttl time.Duration) {
	c.mu.Lock()
	c.entries[code] = claimCodeEntry{userID: userID, expiresAt: time.Now().Add(ttl)}
	c.mu.Unlock()
	go c.cleanup()
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

func (c *memoryClaimCodeCache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for code, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, code)
		}
	}
}
