package intent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type cacheKey string

type cacheEntry struct {
	verdict   *VerificationVerdict
	expiresAt time.Time
}

// verdictCache is a simple in-memory cache for verification verdicts.
type verdictCache struct {
	mu      sync.Mutex
	entries map[cacheKey]cacheEntry
	ttl     time.Duration
}

func newVerdictCache(ttl time.Duration) *verdictCache {
	return &verdictCache{
		entries: make(map[cacheKey]cacheEntry),
		ttl:     ttl,
	}
}

// Get returns a cached verdict if it exists and hasn't expired.
func (c *verdictCache) Get(key cacheKey) (*VerificationVerdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	// Return a copy so callers can mutate (e.g. set Cached=true)
	cp := *entry.verdict
	return &cp, true
}

// Put stores a verdict in the cache.
func (c *verdictCache) Put(key cacheKey, verdict *VerificationVerdict) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = cacheEntry{
		verdict:   verdict,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Cleanup removes expired entries.
func (c *verdictCache) Cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for k, v := range c.entries {
		if now.After(v.expiresAt) {
			delete(c.entries, k)
		}
	}
}

// buildCacheKey builds a cache key from (taskID, service, action, sha256(params), sha256(reason), sha256(chainFacts)?).
func buildCacheKey(req VerifyRequest) cacheKey {
	paramsBytes, _ := json.Marshal(req.Params)
	paramsHash := sha256.Sum256(paramsBytes)
	reasonHash := sha256.Sum256([]byte(req.Reason))

	optOut := "0"
	if req.ChainContextOptOut {
		optOut = "1"
	}
	mode := "s"
	if req.Lenient {
		mode = "l"
	}

	if len(req.ChainFacts) > 0 {
		factsBytes, _ := json.Marshal(req.ChainFacts)
		factsHash := sha256.Sum256(factsBytes)
		return cacheKey(fmt.Sprintf("%s|%s|%s|%x|%x|%x|%s|%s",
			req.TaskID, req.Service, req.Action, paramsHash[:8], reasonHash[:8], factsHash[:8], optOut, mode))
	}

	return cacheKey(fmt.Sprintf("%s|%s|%s|%x|%x|%s|%s",
		req.TaskID, req.Service, req.Action, paramsHash[:8], reasonHash[:8], optOut, mode))
}
