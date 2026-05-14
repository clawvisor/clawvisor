package llmproxy

import (
	"sync"
	"time"
)

// InlineApprovalOutcome records what happened when the proxy attempted
// to create the inline-approved task. The augmenter looks up an
// outcome by approval ID parsed from the prior-assistant prompt and
// injects the matching context onto subsequent turns — claiming
// "task was created" only when the creation actually succeeded.
type InlineApprovalOutcome struct {
	// Succeeded is true when the task was created and the approval
	// record was persisted. False on any failure path (validation,
	// missing creator, store error).
	Succeeded bool
	// TaskID is populated on success.
	TaskID string
	// FailureReason is populated on failure — short, suitable for
	// embedding in an LLM-facing context note.
	FailureReason string
}

// InlineApprovalOutcomeStore persists per-approval outcomes for the
// duration of a conversation. The augmenter relies on the store to
// distinguish a previously-successful approval (re-inject success
// context) from a previously-failed one (re-inject failure context),
// rather than blindly assuming success from the presence of a bare
// "approve" in conversation history.
type InlineApprovalOutcomeStore interface {
	Record(approvalID string, outcome InlineApprovalOutcome)
	Lookup(approvalID string) (InlineApprovalOutcome, bool)
}

// MemoryInlineApprovalOutcomeStore is an in-process outcome store with
// TTL eviction. Outcomes only matter for in-flight conversations, so a
// process-local store is sufficient — daemon restart resets state,
// after which there are no live inline approvals to worry about.
type MemoryInlineApprovalOutcomeStore struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]memoryOutcomeEntry
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
		entries: map[string]memoryOutcomeEntry{},
	}
}

func (s *MemoryInlineApprovalOutcomeStore) Record(approvalID string, outcome InlineApprovalOutcome) {
	if approvalID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(time.Now())
	s.entries[approvalID] = memoryOutcomeEntry{
		outcome:   outcome,
		expiresAt: time.Now().Add(s.ttl),
	}
}

func (s *MemoryInlineApprovalOutcomeStore) Lookup(approvalID string) (InlineApprovalOutcome, bool) {
	if approvalID == "" {
		return InlineApprovalOutcome{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[approvalID]
	if !ok {
		return InlineApprovalOutcome{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(s.entries, approvalID)
		return InlineApprovalOutcome{}, false
	}
	return entry.outcome, true
}

func (s *MemoryInlineApprovalOutcomeStore) gcLocked(now time.Time) {
	for id, entry := range s.entries {
		if now.After(entry.expiresAt) {
			delete(s.entries, id)
		}
	}
}
