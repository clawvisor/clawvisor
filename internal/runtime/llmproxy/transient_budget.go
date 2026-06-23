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

// TransientReleaseToken is the per-consume nonce returned by Try and
// passed to Release. It scopes a Release to the specific consume the
// caller performed — without it, a delayed rollback whose original
// slot has since been pruned and re-consumed by a different request
// would refund the new consumer's slot instead. Treat as opaque.
type TransientReleaseToken uint64

// TransientBudget rations one-shot retries for transient failures
// (LLM judge timeout, nonce-mint hiccup, decision-engine RPC blip).
// The postproc session consults this on every Deny verdict carrying
// a TransientFailureClass: the first occurrence per (agentID,
// conversationID, failureClass) within TTL gets promoted to a
// RecoverableDeny so the agent's continuation retry fires; every
// subsequent occurrence passes through as a plain Deny so a chronic
// failure surfaces to the user instead of looping silently.
//
// Try / Release form a consume / refund pair. The postproc session
// calls Release for every Try it did on a request whose response was
// later fail-closed — otherwise the retry slot would burn for a
// recoverable response the agent never actually saw. Release MUST
// pass the token Try returned so a delayed rollback can't refund a
// slot that has since been re-consumed by a different request.
type TransientBudget interface {
	// Try records an attempt for key. On the FIRST attempt within TTL
	// returns (token, true) — caller should promote to recoverable
	// AND retain the token so it can Release the slot on rollback.
	// On subsequent attempts returns (0, false) — budget exhausted,
	// surface plain Deny.
	Try(ctx context.Context, key TransientBudgetKey) (TransientReleaseToken, bool)
	// Release refunds a previously-successful Try so the slot is
	// available again. No-op when the entry at key carries a different
	// token (the original slot was pruned and re-consumed by a later
	// Try) or when the key isn't currently consumed. Token-checked
	// delete is what makes this safe under a delayed rollback that
	// fires after the slot has rotated.
	Release(ctx context.Context, key TransientBudgetKey, token TransientReleaseToken)
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
		entries: map[TransientBudgetKey]transientBudgetEntry{},
	}
}

type transientBudgetEntry struct {
	expiresAt time.Time
	token     TransientReleaseToken
}

type memoryTransientBudget struct {
	mu          sync.Mutex
	ttl         time.Duration
	now         func() time.Time
	nextTok     uint64 // monotonic source for TransientReleaseToken
	entries     map[TransientBudgetKey]transientBudgetEntry
	lastPruneAt time.Time
}

func (b *memoryTransientBudget) Try(_ context.Context, key TransientBudgetKey) (TransientReleaseToken, bool) {
	if b == nil {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	if entry, exists := b.entries[key]; exists {
		if entry.expiresAt.After(b.now().UTC()) {
			return 0, false
		}
		delete(b.entries, key)
	}
	b.nextTok++
	token := TransientReleaseToken(b.nextTok)
	b.entries[key] = transientBudgetEntry{
		expiresAt: b.now().UTC().Add(b.ttl),
		token:     token,
	}
	return token, true
}

func (b *memoryTransientBudget) Release(_ context.Context, key TransientBudgetKey, token TransientReleaseToken) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[key]
	if !ok || entry.token != token {
		return
	}
	delete(b.entries, key)
}

func (b *memoryTransientBudget) pruneLocked() {
	now := b.now().UTC()
	if now.Sub(b.lastPruneAt) < 30*time.Second {
		return
	}
	b.lastPruneAt = now
	for key, entry := range b.entries {
		if entry.expiresAt.After(now) {
			continue
		}
		delete(b.entries, key)
	}
}
