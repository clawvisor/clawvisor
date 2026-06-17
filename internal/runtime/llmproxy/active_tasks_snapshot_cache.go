package llmproxy

import (
	"sync"
	"time"
)

// ActiveTasksSnapshotKey scopes a cached snapshot to one conversation.
// Anthropic's prompt cache is keyed on raw bytes; the ACTIVE TASKS
// snapshot lives in the system prompt above the cache breakpoints, so
// any drift in the bullet rows (a new task created mid-conversation,
// a stale task dropped) re-keys the cache and forces a 15k+ token
// re-warm on the next turn. Freezing the snapshot for the lifetime of
// the conversation removes that one re-warm per task event — the agent
// still learns about the new task via the augmenter's task-approved
// notice (which lands below the cache breakpoint) and via GET
// /control/tasks when it wants live state.
type ActiveTasksSnapshotKey struct {
	UserID         string
	AgentID        string
	ConversationID string
}

// ActiveTasksSnapshotCache stores the rendered ACTIVE TASKS snapshot
// for one conversation. The control-notice policy consults the cache
// on every turn; the first-turn render persists, and every later turn
// reads it verbatim.
//
// The negative ("") snapshot is also valid: a conversation that
// started with zero active tasks should render the empty-state copy
// on every later turn even if a task is created in between, so the
// system prompt bytes stay stable.
type ActiveTasksSnapshotCache interface {
	Lookup(key ActiveTasksSnapshotKey) (string, bool)
	Record(key ActiveTasksSnapshotKey, snapshot string)
}

// MemoryActiveTasksSnapshotCache is an in-process snapshot cache with
// TTL eviction and a soft size cap. Adequate for single-process
// installs; multi-replica deployments where conversations may land on
// different replicas should wire a shared-backend cache (redis, etc.)
// — even a per-replica memory cache helps because most conversations
// stay sticky to one replica for their lifetime.
type MemoryActiveTasksSnapshotCache struct {
	ttl     time.Duration
	maxSize int

	mu      sync.Mutex
	entries map[ActiveTasksSnapshotKey]activeTasksSnapshotEntry
}

type activeTasksSnapshotEntry struct {
	snapshot  string
	expiresAt time.Time
}

// NewMemoryActiveTasksSnapshotCache constructs the cache. ttl<=0
// defaults to 24h; maxSize<=0 defaults to 10_000 entries.
func NewMemoryActiveTasksSnapshotCache(ttl time.Duration, maxSize int) *MemoryActiveTasksSnapshotCache {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if maxSize <= 0 {
		maxSize = 10_000
	}
	return &MemoryActiveTasksSnapshotCache{
		ttl:     ttl,
		maxSize: maxSize,
		entries: map[ActiveTasksSnapshotKey]activeTasksSnapshotEntry{},
	}
}

func (c *MemoryActiveTasksSnapshotCache) Lookup(key ActiveTasksSnapshotKey) (string, bool) {
	if key.ConversationID == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, key)
		return "", false
	}
	return entry.snapshot, true
}

func (c *MemoryActiveTasksSnapshotCache) Record(key ActiveTasksSnapshotKey, snapshot string) {
	if key.ConversationID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.gcLocked(now)
	if len(c.entries) >= c.maxSize {
		// Soft cap: drop one arbitrary entry to make room. Map
		// iteration order is randomized so this isn't pure LRU, but
		// it bounds memory without dragging in a heap.
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[key] = activeTasksSnapshotEntry{
		snapshot:  snapshot,
		expiresAt: now.Add(c.ttl),
	}
}

func (c *MemoryActiveTasksSnapshotCache) gcLocked(now time.Time) {
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
}
