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

// buildCacheKey builds a cache key from (orgID, taskID, service, action,
// sha256(params), sha256(reason), sha256(chainFacts)?,
// sha256(promptOverride)?, sha256(taskGuidance)?, prompt mode).
//
// The orgID prefix and the override/guidance hashes are critical: per-
// org prompt overrides and natural-language task policies change the
// effective verifier behavior, so verdicts produced under one prompt
// configuration must not be reused under another. Without this, two
// orgs configured to deny vs allow the same task could see swapped
// verdicts.
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
	if req.ProxyLite {
		mode += "p"
	}

	// Promote prompt/guidance into the mode suffix when non-empty so
	// the common case (no overrides) keeps the short key shape.
	var overrideTag string
	if req.PromptOverride != "" {
		h := sha256.Sum256([]byte(req.PromptOverride))
		overrideTag = fmt.Sprintf("|po:%x", h[:8])
	}
	if req.TaskGuidance != "" {
		h := sha256.Sum256([]byte(req.TaskGuidance))
		overrideTag += fmt.Sprintf("|tg:%x", h[:8])
	}

	// OrgID is always prefixed (empty for non-org-scoped requests).
	// Hash before joining: a raw OrgID containing the field separator
	// "|" or the empty-org sentinel "_" could otherwise collide with
	// another orgID or with the empty-org bucket, letting an org-scoped
	// verdict be reused under a different scope. The same defensive
	// hashing pattern is used for PromptOverride/TaskGuidance below.
	var orgPrefix string
	if req.OrgID == "" {
		orgPrefix = "_"
	} else {
		h := sha256.Sum256([]byte(req.OrgID))
		orgPrefix = fmt.Sprintf("%x", h[:8])
	}

	if len(req.ChainFacts) > 0 {
		factsBytes, _ := json.Marshal(req.ChainFacts)
		factsHash := sha256.Sum256(factsBytes)
		return cacheKey(fmt.Sprintf("%s|%s|%s|%s|%x|%x|%x|%s|%s%s",
			orgPrefix, req.TaskID, req.Service, req.Action, paramsHash[:8], reasonHash[:8], factsHash[:8], optOut, mode, overrideTag))
	}

	return cacheKey(fmt.Sprintf("%s|%s|%s|%s|%x|%x|%s|%s%s",
		orgPrefix, req.TaskID, req.Service, req.Action, paramsHash[:8], reasonHash[:8], optOut, mode, overrideTag))
}
