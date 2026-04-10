package handlers

import (
	"sync"
	"time"
)

// TokenCache stores short-lived approved agent tokens. The in-memory
// implementation is used in single-instance mode; a Redis-backed
// implementation is used in multi-instance mode.
type TokenCache interface {
	Store(id string, rawToken string)
	Load(id string) (rawToken string, ok bool)
}

// memoryTokenCache is the default in-memory token cache.
type memoryTokenCache struct {
	mu     sync.RWMutex
	tokens map[string]approvedToken
	window time.Duration
}

func newMemoryTokenCache(window time.Duration) *memoryTokenCache {
	return &memoryTokenCache{
		tokens: make(map[string]approvedToken),
		window: window,
	}
}

func (c *memoryTokenCache) Store(id string, rawToken string) {
	c.mu.Lock()
	c.tokens[id] = approvedToken{raw: rawToken, approvedAt: time.Now()}
	c.mu.Unlock()
	go c.cleanup()
}

func (c *memoryTokenCache) Load(id string) (string, bool) {
	c.mu.RLock()
	tok, ok := c.tokens[id]
	c.mu.RUnlock()
	if !ok || time.Since(tok.approvedAt) > c.window {
		return "", false
	}
	return tok.raw, true
}

func (c *memoryTokenCache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for id, tok := range c.tokens {
		if now.Sub(tok.approvedAt) > c.window {
			delete(c.tokens, id)
		}
	}
}
