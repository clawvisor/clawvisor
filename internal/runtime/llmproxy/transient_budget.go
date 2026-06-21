package llmproxy

import (
	"context"
	"sync"
	"time"
)

// TransientBudgetKey identifies a single transient-retry slot. Struct
// (not concatenated string) so distinct (agent, conversation, class)
// tuples never alias each other regardless of what characters the IDs
// contain — string concatenation with any separator would collide
// when a component happens to contain the separator.
type TransientBudgetKey struct {
	AgentID        string
	ConversationID string
	FailureClass   string
}

// TransientBudget rations one-shot retries for transient failures
// (LLM judge timeout, nonce-mint hiccup, decision-engine RPC blip).
// The postproc transient transform consults this on every Deny
// verdict carrying a TransientFailureClass: the first occurrence per
// (agentID, conversationID, failureClass) within TTL gets promoted to
// a RecoverableDeny so the agent's continuation retry fires; every
// subsequent occurrence passes through as a plain Deny so a chronic
// failure surfaces to the user instead of looping silently.
//
// Try / Release form a consume / refund pair. The postproc session
// calls Release for every Try it did on a request whose response was
// later fail-closed — otherwise the retry slot would burn for a
// recoverable response the agent never actually saw.
type TransientBudget interface {
	// Try records an attempt for key. Returns true on the FIRST
	// attempt within TTL (budget remaining, caller should promote to
	// recoverable). Returns false on subsequent attempts (budget
	// exhausted, surface plain Deny).
	Try(ctx context.Context, key TransientBudgetKey) bool
	// Release refunds a previously-successful Try so the slot is
	// available again. No-op when the key isn't currently consumed
	// (already released or expired). Used by postproc rollback so a
	// fail-closed response doesn't burn the agent's one retry slot.
	Release(ctx context.Context, key TransientBudgetKey)
}

// NewMemoryTransientBudget returns an in-memory TransientBudget.
// TTL <= 0 falls back to 5 minutes — shorter than the 10-minute
// ScopeDrifts default because transient classes should rotate fast: a
// stale "judge timeout" record shouldn't block recovery for a fresh
// tool_use much later in the same conversation.
func NewMemoryTransientBudget(ttl time.Duration) TransientBudget {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &memoryTransientBudget{
		ttl:     ttl,
		now:     time.Now,
		entries: map[TransientBudgetKey]time.Time{},
	}
}

type memoryTransientBudget struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[TransientBudgetKey]time.Time
}

func (b *memoryTransientBudget) Try(_ context.Context, key TransientBudgetKey) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	if _, exists := b.entries[key]; exists {
		return false
	}
	b.entries[key] = b.now().UTC().Add(b.ttl)
	return true
}

func (b *memoryTransientBudget) Release(_ context.Context, key TransientBudgetKey) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, key)
}

func (b *memoryTransientBudget) pruneLocked() {
	now := b.now().UTC()
	for key, expires := range b.entries {
		if expires.After(now) {
			continue
		}
		delete(b.entries, key)
	}
}
