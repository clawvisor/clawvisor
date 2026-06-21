package llmproxy

import (
	"context"
	"sync"
	"time"
)

// TransientBudget rations one-shot retries for transient failures
// (LLM judge timeout, nonce-mint hiccup, decision-engine RPC blip).
// The postproc transient transform consults this on every Deny
// verdict carrying a TransientFailureClass: the first occurrence per
// (agentID, conversationID, failureClass) within TTL gets promoted to
// a RecoverableDeny so the agent's continuation retry fires; every
// subsequent occurrence passes through as a plain Deny so a chronic
// failure surfaces to the user instead of looping silently.
type TransientBudget interface {
	// Try records an attempt for (agentID, conversationID, failureClass).
	// Returns true on the FIRST attempt within TTL (budget remaining,
	// caller should promote to recoverable). Returns false on
	// subsequent attempts (budget exhausted, surface plain Deny).
	Try(ctx context.Context, agentID, conversationID, failureClass string) bool
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
		entries: map[string]time.Time{},
	}
}

type memoryTransientBudget struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]time.Time
}

func (b *memoryTransientBudget) Try(_ context.Context, agentID, conversationID, failureClass string) bool {
	if b == nil {
		return false
	}
	key := transientBudgetKey(agentID, conversationID, failureClass)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	if _, exists := b.entries[key]; exists {
		return false
	}
	b.entries[key] = b.now().UTC().Add(b.ttl)
	return true
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

func transientBudgetKey(agentID, conversationID, failureClass string) string {
	return agentID + "|" + conversationID + "|" + failureClass
}
