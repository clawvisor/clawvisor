package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestListExpiredInlineChatPendingTasks_CutoffFormatRespectsSQLiteDatetime
// guards against a regression where the cutoff was formatted as RFC3339
// (e.g. "2026-05-28T08:30:00Z") and compared lexicographically against
// the sqlite-native "YYYY-MM-DD HH:MM:SS" stored in created_at. A
// freshly-created row's stored format begins "2026-05-28 ..."; RFC3339's
// 'T' (0x54) sorts higher than ' ' (0x20), so the row's stored value
// is unconditionally less than any same-day RFC3339 cutoff — the sweep
// would auto-deny tasks created seconds ago. The cutoff must use the
// same "YYYY-MM-DD HH:MM:SS" shape so the byte-wise compare is
// chronologically honest.
func TestListExpiredInlineChatPendingTasks_CutoffFormatRespectsSQLiteDatetime(t *testing.T) {
	ctx := context.Background()
	db, err := New(ctx, filepath.Join(t.TempDir(), "expiry.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db)

	user, err := st.CreateUser(ctx, "expiry@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "tokhash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Fresh chat-bound pending task. created_at lands at the column's
	// default (datetime('now')) → "YYYY-MM-DD HH:MM:SS".
	task := &store.Task{
		ID:                "task-fresh",
		UserID:            user.ID,
		AgentID:           agent.ID,
		Purpose:           "fresh",
		Status:            "pending_approval",
		Lifetime:          "session",
		ApprovalSource:    "inline_chat",
		AuthorizedActions: nil,
		ExpiresInSeconds:  600,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Cutoff 1h in the past: the row is seconds old, so it must NOT be
	// returned. Pre-fix, the RFC3339-formatted cutoff produced a
	// lexicographic mis-compare and the row WAS returned, which would
	// have caused the sweeper to immediately auto-deny every freshly
	// created chat-bound pending row.
	got, err := st.ListExpiredInlineChatPendingTasks(ctx, time.Now().UTC().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("ListExpiredInlineChatPendingTasks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fresh chat-bound pending task must not be flagged expired with a 1h-past cutoff; got %d rows", len(got))
	}

	// Future cutoff: the row's created_at is BEFORE the cutoff, so it
	// MUST be returned. Confirms the format is correctly chronological,
	// not just "always empty."
	got, err = st.ListExpiredInlineChatPendingTasks(ctx, time.Now().UTC().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("ListExpiredInlineChatPendingTasks: %v", err)
	}
	if len(got) != 1 || got[0].ID != "task-fresh" {
		t.Fatalf("future cutoff must return the chat-bound row; got %d rows %+v", len(got), got)
	}
}
