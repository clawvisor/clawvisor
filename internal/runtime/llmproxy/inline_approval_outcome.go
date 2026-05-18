package llmproxy

import (
	"sync"
	"time"
)

// InlineApprovalOutcome is the canonical in-memory resolution record
// for one inline task approval. The augmenter uses Succeeded/TaskID/
// FailureReason to render later conversation history, while audit and
// diagnostics can correlate the same approval ID to the decision,
// outcome, request, and approval_records row that the rewrite path
// produced. Keeping this as one record avoids maintaining a separate
// "augmenter outcome" fact beside the audit fact.
type InlineApprovalOutcome struct {
	// Decision is the audit-level decision: "allow" on successful
	// task creation, "deny" on denial or failure.
	Decision string
	// Outcome is the short audit/event tag, e.g. "inline_task_approved"
	// or "inline_task_create_failed".
	Outcome string
	// Succeeded is true when the task was created and the approval
	// record was persisted. False on any failure path (validation,
	// missing creator, store error).
	Succeeded bool
	// TaskID is populated on success.
	TaskID string
	// Credentials are the concrete placeholders minted for
	// required_credentials entries when the task was approved.
	Credentials []InlineTaskCredentialPlaceholder
	// ApprovalRecordID is populated on success when the canonical
	// approval_records row was written.
	ApprovalRecordID string
	// FailureReason is populated on failure — short, suitable for
	// embedding in an LLM-facing context note.
	FailureReason string
	// RequestID links this resolution back to the lite-proxy request
	// that processed the user's yes/no reply.
	RequestID string
	// ResolvedAt is when the proxy resolved the inline approval.
	ResolvedAt time.Time
}

// InlineApprovalOutcomeKey scopes an outcome record. The approval ID
// alone is unguessable in practice (16 random bytes), but every other
// approval-related store in this codebase scopes by user/agent for
// defense in depth. Pinning outcomes to (UserID, AgentID, ApprovalID)
// rules out a model in agent B's session influencing the augmenter
// for agent A by replaying a known approval ID — purely a model-
// confusion vector, since real authorization runs against the task
// store, but consistent with the rest of the codebase's scoping.
type InlineApprovalOutcomeKey struct {
	UserID     string
	AgentID    string
	ApprovalID string
}

// InlineApprovalOutcomeStore persists per-approval outcomes for the
// duration of a conversation. The augmenter relies on the store to
// distinguish a previously-successful approval (re-inject success
// context) from a previously-failed one (re-inject failure context),
// rather than blindly assuming success from the presence of a bare
// "approve" in conversation history.
type InlineApprovalOutcomeStore interface {
	Record(key InlineApprovalOutcomeKey, outcome InlineApprovalOutcome)
	Lookup(key InlineApprovalOutcomeKey) (InlineApprovalOutcome, bool)
}

// MemoryInlineApprovalOutcomeStore is an in-process outcome store with
// TTL eviction. Outcomes only matter for in-flight conversations, so a
// process-local store is sufficient — daemon restart resets state,
// after which there are no live inline approvals to worry about.
type MemoryInlineApprovalOutcomeStore struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[InlineApprovalOutcomeKey]memoryOutcomeEntry
}

type memoryOutcomeEntry struct {
	outcome   InlineApprovalOutcome
	expiresAt time.Time
}

func NewMemoryInlineApprovalOutcomeStore(ttl time.Duration) *MemoryInlineApprovalOutcomeStore {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &MemoryInlineApprovalOutcomeStore{
		ttl:     ttl,
		entries: map[InlineApprovalOutcomeKey]memoryOutcomeEntry{},
	}
}

func (s *MemoryInlineApprovalOutcomeStore) Record(key InlineApprovalOutcomeKey, outcome InlineApprovalOutcome) {
	if key.ApprovalID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(time.Now())
	s.entries[key] = memoryOutcomeEntry{
		outcome:   outcome,
		expiresAt: time.Now().Add(s.ttl),
	}
}

func (s *MemoryInlineApprovalOutcomeStore) Lookup(key InlineApprovalOutcomeKey) (InlineApprovalOutcome, bool) {
	if key.ApprovalID == "" {
		return InlineApprovalOutcome{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return InlineApprovalOutcome{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(s.entries, key)
		return InlineApprovalOutcome{}, false
	}
	return entry.outcome, true
}

func (s *MemoryInlineApprovalOutcomeStore) gcLocked(now time.Time) {
	for key, entry := range s.entries {
		if now.After(entry.expiresAt) {
			delete(s.entries, key)
		}
	}
}
