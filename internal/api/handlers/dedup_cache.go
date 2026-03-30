package handlers

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type dedupKey string

type dedupEntry struct {
	value     any
	expiresAt time.Time
}

// dedupCache is a short-lived, in-memory cache keyed by content hash.
// It deduplicates identical requests that arrive within a configurable TTL window.
type dedupCache struct {
	mu      sync.Mutex
	entries map[dedupKey]dedupEntry
	ttl     time.Duration
	done    chan struct{}
}

func newDedupCache(ttl time.Duration) *dedupCache {
	c := &dedupCache{
		entries: make(map[dedupKey]dedupEntry),
		ttl:     ttl,
		done:    make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

// Get returns a cached value if one exists and hasn't expired.
func (c *dedupCache) Get(key dedupKey) (any, bool) {
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
	return entry.value, true
}

// Put stores a value in the cache.
func (c *dedupCache) Put(key dedupKey, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = dedupEntry{
		value:     value,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Stop stops the background cleanup goroutine.
func (c *dedupCache) Stop() {
	close(c.done)
}

func (c *dedupCache) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.cleanup()
		case <-c.done:
			return
		}
	}
}

func (c *dedupCache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for k, v := range c.entries {
		if now.After(v.expiresAt) {
			delete(c.entries, k)
		}
	}
}

// buildDedupKey builds a cache key by hashing all provided parts.
func buildDedupKey(parts ...any) dedupKey {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return dedupKey(fmt.Sprintf("%x", h[:16]))
}
