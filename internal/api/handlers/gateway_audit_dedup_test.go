package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// raceRecoveryStore stages a simulated unique-violation race: the first
// LogAudit call returns ErrConflict; the next FindDedupCandidate returns the
// "winner" canonical; the second LogAudit call (the rewritten dedup-attempt
// row) succeeds.
type raceRecoveryStore struct {
	store.Store
	winner *store.AuditEntry

	callsLogAudit       int
	callsFindCandidate  int
	lastInserted        *store.AuditEntry
	failFirstLogAuditAs error
	failCandidateAs     error
}

func (s *raceRecoveryStore) LogAudit(_ context.Context, e *store.AuditEntry) error {
	s.callsLogAudit++
	if s.callsLogAudit == 1 && s.failFirstLogAuditAs != nil {
		return s.failFirstLogAuditAs
	}
	// On the recovery insert, capture the rewritten entry for assertions.
	copyOf := *e
	s.lastInserted = &copyOf
	return nil
}

func (s *raceRecoveryStore) FindDedupCandidate(_ context.Context, _, _, _ string) (*store.AuditEntry, error) {
	s.callsFindCandidate++
	if s.failCandidateAs != nil {
		return nil, s.failCandidateAs
	}
	return s.winner, nil
}

// TestLogAuditCanonical_RaceRecovery covers the unique-violation race: when
// two workers both miss FindDedupCandidate and both try to insert canonical,
// the loser must rewrite its row as a dedup-attempt pointing at the winner.
// The mutated row's Decision/Outcome must match the canonical's outcome so
// the audit UI shows a consistent story.
func TestLogAuditCanonical_RaceRecovery(t *testing.T) {
	winnerTaskID := "task-A"
	winner := &store.AuditEntry{
		ID:       "winner-id",
		UserID:   "user-1",
		TaskID:   &winnerTaskID,
		Decision: "execute",
		Outcome:  "executed",
	}
	st := &raceRecoveryStore{
		winner:              winner,
		failFirstLogAuditAs: store.ErrConflict,
	}
	h := &GatewayHandler{store: st}

	loserTaskID := winnerTaskID
	loser := &store.AuditEntry{
		ID:        "loser-id",
		UserID:    "user-1",
		RequestID: "req-1",
		TaskID:    &loserTaskID,
		Decision:  "execute",
		Outcome:   "executed",
	}

	if _, err := h.logAuditCanonical(context.Background(), loser); err != nil {
		t.Fatalf("logAuditCanonical: %v", err)
	}

	if st.callsLogAudit != 2 {
		t.Errorf("expected 2 LogAudit calls (1 conflict + 1 attempt), got %d", st.callsLogAudit)
	}
	if st.callsFindCandidate != 1 {
		t.Errorf("expected 1 FindDedupCandidate call, got %d", st.callsFindCandidate)
	}
	if st.lastInserted == nil {
		t.Fatal("expected a recovery LogAudit insert, got none")
	}
	if st.lastInserted.DedupedOf == nil || *st.lastInserted.DedupedOf != winner.ID {
		t.Errorf("DedupedOf: got %v, want pointer to %q", st.lastInserted.DedupedOf, winner.ID)
	}
	if st.lastInserted.Decision != "dedup" {
		t.Errorf("Decision: got %q, want %q", st.lastInserted.Decision, "dedup")
	}
	if st.lastInserted.Outcome != winner.Outcome {
		t.Errorf("Outcome: got %q, want %q (snapshot of canonical)", st.lastInserted.Outcome, winner.Outcome)
	}
	if st.lastInserted.ID != loser.ID {
		t.Errorf("attempt row ID: got %q, want %q (loser's own audit ID)", st.lastInserted.ID, loser.ID)
	}
}

// TestLogAuditCanonical_NonConflictError ensures non-conflict errors propagate
// directly — only ErrConflict triggers race recovery.
func TestLogAuditCanonical_NonConflictError(t *testing.T) {
	dbErr := errors.New("database is down")
	st := &raceRecoveryStore{failFirstLogAuditAs: dbErr}
	h := &GatewayHandler{store: st}

	taskID := "task-A"
	e := &store.AuditEntry{ID: "id-1", UserID: "u", RequestID: "r", TaskID: &taskID, Decision: "execute", Outcome: "executed"}
	_, err := h.logAuditCanonical(context.Background(), e)
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected db error to propagate, got %v", err)
	}
	if st.callsFindCandidate != 0 {
		t.Errorf("FindDedupCandidate should not be called for non-conflict errors, got %d", st.callsFindCandidate)
	}
}

// TestLogAuditCanonical_HappyPath verifies the no-conflict case: a single
// successful LogAudit with no candidate lookup.
func TestLogAuditCanonical_HappyPath(t *testing.T) {
	st := &raceRecoveryStore{} // no failure staged
	h := &GatewayHandler{store: st}

	taskID := "task-A"
	e := &store.AuditEntry{ID: "id-1", UserID: "u", RequestID: "r", TaskID: &taskID, Decision: "execute", Outcome: "executed"}
	if _, err := h.logAuditCanonical(context.Background(), e); err != nil {
		t.Fatalf("logAuditCanonical: %v", err)
	}
	if st.callsLogAudit != 1 {
		t.Errorf("expected 1 LogAudit call, got %d", st.callsLogAudit)
	}
	if st.callsFindCandidate != 0 {
		t.Errorf("expected 0 FindDedupCandidate calls, got %d", st.callsFindCandidate)
	}
	if e.DedupedOf != nil {
		t.Errorf("happy path should not mutate DedupedOf, got %v", e.DedupedOf)
	}
	if e.Decision != "execute" {
		t.Errorf("happy path should not mutate Decision, got %q", e.Decision)
	}
}
