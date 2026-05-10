package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// auditDedupFixture sets up a user/agent and returns the store ready for
// audit_log writes scoped to that user.
type auditDedupFixture struct {
	st     *Store
	userID string
	agent  *store.Agent
}

func newAuditDedupFixture(t *testing.T) *auditDedupFixture {
	t.Helper()
	ctx := context.Background()
	db, err := New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db)
	user, err := st.CreateUser(ctx, "audit-dedup@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "audit-dedup-agent", "tok")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return &auditDedupFixture{st: st, userID: user.ID, agent: agent}
}

func (f *auditDedupFixture) makeEntry(id, requestID string, taskID *string, decision, outcome string, ts time.Time) *store.AuditEntry {
	return &store.AuditEntry{
		ID:         id,
		UserID:     f.userID,
		AgentID:    &f.agent.ID,
		RequestID:  requestID,
		TaskID:     taskID,
		Timestamp:  ts,
		Service:    "mock.svc",
		Action:     "run",
		ParamsSafe: []byte(`{}`),
		Decision:   decision,
		Outcome:    outcome,
	}
}

// TestAuditDedup_PerScopeUniqueConstraint asserts that two canonical inserts
// with the same (user_id, request_id, task_id) collide, and that a third
// canonical for the same request_id but a different task_id lands cleanly.
func TestAuditDedup_PerScopeUniqueConstraint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuditDedupFixture(t)

	now := time.Now().UTC().Truncate(time.Second)
	taskA, taskB := "task-A", "task-B"

	// First canonical for (request, taskA) — succeeds.
	first := f.makeEntry("audit-1", "req-1", &taskA, "execute", "executed", now)
	if err := f.st.LogAudit(ctx, first); err != nil {
		t.Fatalf("LogAudit first: %v", err)
	}

	// Second canonical for the same scope — must collide with ErrConflict.
	collide := f.makeEntry("audit-2", "req-1", &taskA, "execute", "executed", now.Add(time.Second))
	err := f.st.LogAudit(ctx, collide)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("LogAudit duplicate: got %v, want ErrConflict", err)
	}

	// Same request_id under a different task — different scope, should land.
	crossTask := f.makeEntry("audit-3", "req-1", &taskB, "execute", "executed", now.Add(2*time.Second))
	if err := f.st.LogAudit(ctx, crossTask); err != nil {
		t.Fatalf("LogAudit cross-task: %v", err)
	}

	// Dedup-attempt row (deduped_of != NULL) is outside the partial unique
	// index — many can coexist for the same scope.
	for i := range 3 {
		attempt := f.makeEntry("attempt-"+string(rune('a'+i)), "req-1", &taskA, "dedup", "executed", now.Add(time.Duration(3+i)*time.Second))
		attempt.DedupedOf = &first.ID
		if err := f.st.LogAudit(ctx, attempt); err != nil {
			t.Fatalf("LogAudit attempt %d: %v", i, err)
		}
	}
}

// TestAuditDedup_FindDedupCandidatePrecedence asserts the read-time precedence:
// a pre-task canonical (task_id IS NULL) wins over any task-scoped canonical
// for the same request_id. Within a tier, oldest wins.
func TestAuditDedup_FindDedupCandidatePrecedence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuditDedupFixture(t)

	now := time.Now().UTC().Truncate(time.Second)
	taskA := "task-A"

	// Land a task-scoped canonical first.
	taskScoped := f.makeEntry("audit-task", "req-1", &taskA, "execute", "executed", now)
	if err := f.st.LogAudit(ctx, taskScoped); err != nil {
		t.Fatalf("LogAudit task-scoped: %v", err)
	}

	// FindDedupCandidate(taskID=taskA) should return the task-scoped row.
	got, err := f.st.FindDedupCandidate(ctx, "req-1", f.userID, taskA)
	if err != nil {
		t.Fatalf("FindDedupCandidate task-scoped: %v", err)
	}
	if got.ID != taskScoped.ID {
		t.Errorf("FindDedupCandidate: got %q, want %q", got.ID, taskScoped.ID)
	}

	// Now land a pre-task canonical for the same request_id (task_id IS NULL).
	// The partial unique index treats these as different scopes, so insert succeeds.
	preTask := f.makeEntry("audit-pre", "req-1", nil, "block", "blocked", now.Add(time.Second))
	if err := f.st.LogAudit(ctx, preTask); err != nil {
		t.Fatalf("LogAudit pre-task: %v", err)
	}

	// Pre-task wins over task-scoped: even though the task-scoped row is older,
	// FindDedupCandidate(taskID=taskA) should now return the pre-task row.
	got, err = f.st.FindDedupCandidate(ctx, "req-1", f.userID, taskA)
	if err != nil {
		t.Fatalf("FindDedupCandidate after pre-task: %v", err)
	}
	if got.ID != preTask.ID {
		t.Errorf("FindDedupCandidate after pre-task: got %q, want %q (pre-task should win)", got.ID, preTask.ID)
	}

	// taskID == "" means no task context — only pre-task rows match.
	got, err = f.st.FindDedupCandidate(ctx, "req-1", f.userID, "")
	if err != nil {
		t.Fatalf("FindDedupCandidate no-task: %v", err)
	}
	if got.ID != preTask.ID {
		t.Errorf("FindDedupCandidate no-task: got %q, want %q", got.ID, preTask.ID)
	}

	// GetAuditEntryByRequestIDAndTask inverts precedence vs FindDedupCandidate:
	// the feedback handler wants the row that fired *in the agent's task*, so
	// a task-scoped match wins over a pre-task canonical. Pre-task is only
	// returned as a fallback when no task-scoped row exists.
	got, err = f.st.GetAuditEntryByRequestIDAndTask(ctx, "req-1", f.userID, taskA)
	if err != nil {
		t.Fatalf("GetAuditEntryByRequestIDAndTask: %v", err)
	}
	if got.ID != taskScoped.ID {
		t.Errorf("GetAuditEntryByRequestIDAndTask: got %q, want %q (task-scoped should win)", got.ID, taskScoped.ID)
	}

	// With taskID="" only pre-task matches; it wins by being the only candidate.
	got, err = f.st.GetAuditEntryByRequestIDAndTask(ctx, "req-1", f.userID, "")
	if err != nil {
		t.Fatalf("GetAuditEntryByRequestIDAndTask no-task: %v", err)
	}
	if got.ID != preTask.ID {
		t.Errorf("GetAuditEntryByRequestIDAndTask no-task: got %q, want %q (pre-task fallback)", got.ID, preTask.ID)
	}
}

// TestAuditDedup_GetAuditEntryByRequestID_LatestCanonical asserts that the
// polling endpoint returns the most recent canonical row, never a dedup
// attempt — agents poll right after submitting and want the result of their
// latest canonical attempt.
func TestAuditDedup_GetAuditEntryByRequestID_LatestCanonical(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuditDedupFixture(t)

	now := time.Now().UTC().Truncate(time.Second)
	taskA, taskB := "task-A", "task-B"

	older := f.makeEntry("audit-older", "req-1", &taskA, "execute", "executed", now)
	if err := f.st.LogAudit(ctx, older); err != nil {
		t.Fatalf("LogAudit older: %v", err)
	}
	newer := f.makeEntry("audit-newer", "req-1", &taskB, "block", "blocked", now.Add(time.Second))
	if err := f.st.LogAudit(ctx, newer); err != nil {
		t.Fatalf("LogAudit newer: %v", err)
	}
	attempt := f.makeEntry("audit-attempt", "req-1", &taskB, "dedup", "blocked", now.Add(2*time.Second))
	attempt.DedupedOf = &newer.ID
	if err := f.st.LogAudit(ctx, attempt); err != nil {
		t.Fatalf("LogAudit attempt: %v", err)
	}

	got, err := f.st.GetAuditEntryByRequestID(ctx, "req-1", f.userID)
	if err != nil {
		t.Fatalf("GetAuditEntryByRequestID: %v", err)
	}
	// Skips the attempt row (deduped_of != NULL); returns newest canonical.
	if got.ID != newer.ID {
		t.Errorf("GetAuditEntryByRequestID: got %q, want %q (newest canonical)", got.ID, newer.ID)
	}
}

// TestAuditDedup_MarkAuditDeduped covers the demote-canonical-to-attempt path
// used when a downstream conflict (e.g. approval-table uniqueness) makes a
// just-inserted canonical the wrong row to surface for polling. After the
// demotion, GetAuditEntryByRequestID must skip the demoted row and return the
// canonical it now points at.
func TestAuditDedup_MarkAuditDeduped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuditDedupFixture(t)

	now := time.Now().UTC().Truncate(time.Second)
	taskA, taskB := "task-A", "task-B"

	// Existing canonical (pre-task-1 approval, still pending).
	canonical := f.makeEntry("canonical", "req-1", &taskA, "approve", "pending", now)
	if err := f.st.LogAudit(ctx, canonical); err != nil {
		t.Fatalf("LogAudit canonical: %v", err)
	}

	// Cross-task duplicate canonical that loses the approval-table race.
	loser := f.makeEntry("loser", "req-1", &taskB, "approve", "pending", now.Add(time.Second))
	if err := f.st.LogAudit(ctx, loser); err != nil {
		t.Fatalf("LogAudit loser: %v", err)
	}

	// Demote loser to dedup-attempt pointing at the canonical, snapshotting
	// the canonical's outcome.
	if err := f.st.MarkAuditDeduped(ctx, loser.ID, canonical.ID, canonical.Outcome); err != nil {
		t.Fatalf("MarkAuditDeduped: %v", err)
	}

	// Polling by request_id must now return the canonical, not the demoted row.
	got, err := f.st.GetAuditEntryByRequestID(ctx, "req-1", f.userID)
	if err != nil {
		t.Fatalf("GetAuditEntryByRequestID: %v", err)
	}
	if got.ID != canonical.ID {
		t.Errorf("GetAuditEntryByRequestID: got %q, want %q (demoted row should be skipped)", got.ID, canonical.ID)
	}

	// The demoted row itself must reflect the new shape.
	demoted, err := f.st.GetAuditEntry(ctx, loser.ID, f.userID)
	if err != nil {
		t.Fatalf("GetAuditEntry: %v", err)
	}
	if demoted.Decision != "dedup" {
		t.Errorf("Decision: got %q, want %q", demoted.Decision, "dedup")
	}
	if demoted.Outcome != canonical.Outcome {
		t.Errorf("Outcome: got %q, want %q (snapshot)", demoted.Outcome, canonical.Outcome)
	}
	if demoted.DedupedOf == nil || *demoted.DedupedOf != canonical.ID {
		t.Errorf("DedupedOf: got %v, want pointer to %q", demoted.DedupedOf, canonical.ID)
	}
}

// TestAuditDedup_ApprovalConflictSiblingLookup pins down the lookup the handler
// must use when recovering from an approval-table conflict. The approval-conflict
// recovery path inserts a fresh canonical *before* it discovers the conflict, so
// the just-inserted row is the newest by timestamp. GetAuditEntryByRequestID
// would return the just-inserted row and produce a self-loop; the handler must
// resolve the sibling via pending_approvals.audit_id so polling lands on the row
// that actually owns the live approval.
func TestAuditDedup_ApprovalConflictSiblingLookup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuditDedupFixture(t)

	now := time.Now().UTC().Truncate(time.Second)
	taskA, taskB := "task-A", "task-B"

	// Sibling canonical that already owns a pending approval (older by timestamp).
	sibling := f.makeEntry("sibling-canonical", "req-1", &taskA, "approve", "pending", now)
	if err := f.st.LogAudit(ctx, sibling); err != nil {
		t.Fatalf("LogAudit sibling: %v", err)
	}
	pa := &store.PendingApproval{
		ID:        "pending-1",
		UserID:    f.userID,
		RequestID: "req-1",
		AuditID:   sibling.ID,
		Status:    "pending",
		ExpiresAt: now.Add(time.Hour),
	}
	if err := f.st.SavePendingApproval(ctx, pa); err != nil {
		t.Fatalf("SavePendingApproval: %v", err)
	}

	// Loser canonical inserted by the handler before it discovered the
	// approval-table conflict — newer by timestamp, same request_id, different task.
	loser := f.makeEntry("loser-canonical", "req-1", &taskB, "approve", "pending", now.Add(time.Second))
	if err := f.st.LogAudit(ctx, loser); err != nil {
		t.Fatalf("LogAudit loser: %v", err)
	}

	// The newest-by-timestamp lookup picks the wrong row — this is the bug
	// the handler avoids by going through pending_approvals instead.
	wrong, err := f.st.GetAuditEntryByRequestID(ctx, "req-1", f.userID)
	if err != nil {
		t.Fatalf("GetAuditEntryByRequestID: %v", err)
	}
	if wrong.ID != loser.ID {
		t.Fatalf("setup invariant: GetAuditEntryByRequestID should pick the newest (loser) row, got %q", wrong.ID)
	}

	// The correct lookup: pending_approvals.audit_id → GetAuditEntry. This is
	// what the handler does and must keep doing.
	got, err := f.st.GetPendingApproval(ctx, "req-1")
	if err != nil {
		t.Fatalf("GetPendingApproval: %v", err)
	}
	siblingFound, err := f.st.GetAuditEntry(ctx, got.AuditID, f.userID)
	if err != nil {
		t.Fatalf("GetAuditEntry: %v", err)
	}
	if siblingFound.ID != sibling.ID {
		t.Errorf("sibling lookup: got %q, want %q (must resolve via pending_approvals.audit_id, not timestamp)", siblingFound.ID, sibling.ID)
	}
	if siblingFound.ID == loser.ID {
		t.Error("sibling lookup returned the loser row — would self-loop in MarkAuditDeduped")
	}
}

// TestAuditDedup_ResolvedApprovalConflictFallback covers the lifecycle
// asymmetry: pending_approvals is deleted on deny/expire/execute but
// approval_records.request_id stays populated indefinitely. A later cross-task
// reuse of the same request_id hits ErrConflict on approval_records, by which
// time GetPendingApproval returns nothing — the recovery helper must fall back
// through GetApprovalRecordByRequestID + GetAuditEntryByRequestIDAndTask
// (scoped to the resolved record's task_id) to land on the resolved sibling
// canonical, not on the just-inserted loser.
func TestAuditDedup_ResolvedApprovalConflictFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAuditDedupFixture(t)

	now := time.Now().UTC().Truncate(time.Second)
	taskB := "task-B"

	// Resolved sibling canonical: pre-task approval that was approved + executed
	// long ago. Pre-task (task_id=NULL) keeps the test free of the tasks(id)
	// FK while still exercising the resolved-record lookup chain.
	sibling := f.makeEntry("sibling-canonical", "req-1", nil, "approve", "executed", now)
	if err := f.st.LogAudit(ctx, sibling); err != nil {
		t.Fatalf("LogAudit sibling: %v", err)
	}
	reqID := "req-1"
	expiresAt := now.Add(time.Hour)
	resolvedAt := now.Add(time.Minute)
	rec := &store.ApprovalRecord{
		ID:                  "approval-rec-1",
		Kind:                "request_once",
		UserID:              f.userID,
		RequestID:           &reqID,
		Status:              "approved",
		Surface:             "dashboard",
		ResolutionTransport: "execute_pending_request",
		ExpiresAt:           &expiresAt,
		ResolvedAt:          &resolvedAt,
		Resolution:          "approved",
	}
	if err := f.st.CreateApprovalRecord(ctx, rec); err != nil {
		t.Fatalf("CreateApprovalRecord: %v", err)
	}
	// Note: no pending_approvals row — it was deleted on resolve. This is the
	// scenario the lifecycle asymmetry creates.

	// Loser canonical: cross-task reuse much later, in a different task.
	loser := f.makeEntry("loser-canonical", "req-1", &taskB, "approve", "pending", now.Add(time.Hour))
	if err := f.st.LogAudit(ctx, loser); err != nil {
		t.Fatalf("LogAudit loser: %v", err)
	}

	// First-try lookup (live pending) returns nothing.
	if _, err := f.st.GetPendingApproval(ctx, "req-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetPendingApproval: expected ErrNotFound, got %v", err)
	}

	// Second-try lookup must land on the resolved sibling, not on the loser.
	got, err := f.st.GetApprovalRecordByRequestID(ctx, "req-1")
	if err != nil {
		t.Fatalf("GetApprovalRecordByRequestID: %v", err)
	}
	recTaskID := ""
	if got.TaskID != nil {
		recTaskID = *got.TaskID
	}
	siblingFound, err := f.st.GetAuditEntryByRequestIDAndTask(ctx, "req-1", f.userID, recTaskID)
	if err != nil {
		t.Fatalf("GetAuditEntryByRequestIDAndTask: %v", err)
	}
	if siblingFound.ID != sibling.ID {
		t.Errorf("resolved-record fallback: got %q, want %q", siblingFound.ID, sibling.ID)
	}
	if siblingFound.ID == loser.ID {
		t.Error("resolved-record fallback returned the loser — would self-loop")
	}
}
