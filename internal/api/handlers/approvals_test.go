package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

// TestExpireTimedOut_StrandedExecutorPreservesApprovedCanonical guards against
// the "illegal canonical approval transition" page. The stranded-executing
// recovery sweep used to pipe its pending-approval row through the same helper
// as the never-replied path, which unconditionally tried to flip the canonical
// record to deny/expired. For a stranded request the canonical record is
// already in "approved" (the user said yes; only execution lapsed), so the
// transition validator rejected it and logged an Error-level line that fired
// the alert. The user's recorded decision must stand, and the recovery sweep
// must not produce that error.
func TestExpireTimedOut_StrandedExecutorPreservesApprovedCanonical(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "stranded-canonical.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "stranded-canonical@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	requestID := "req-stranded-1"
	rec := &store.ApprovalRecord{
		ID:        "rec-stranded-1",
		Kind:      "request_once",
		UserID:    user.ID,
		AgentID:   &agent.ID,
		RequestID: &requestID,
		Status:    "pending",
		Surface:   "dashboard",
	}
	if err := st.CreateApprovalRecord(ctx, rec); err != nil {
		t.Fatalf("CreateApprovalRecord: %v", err)
	}

	// Simulate the user clicking Approve: canonical record becomes "approved".
	if err := st.ResolveApprovalRecord(ctx, rec.ID, "allow_once", "approved", time.Now().UTC()); err != nil {
		t.Fatalf("ResolveApprovalRecord: %v", err)
	}

	// Pending approval row in the state the stranded sweep finds it: the user
	// already approved, the executor claimed it, then crashed past the lease.
	// processExpiredApproval expects the CAS DELETE has already happened, so
	// we don't insert a pending_approvals row — we only construct the struct
	// the sweeper would hand us.
	recID := rec.ID
	pa := &store.PendingApproval{
		UserID:           user.ID,
		RequestID:        requestID,
		AuditID:          "audit-stranded-1",
		ApprovalRecordID: &recID,
		RequestBlob:      []byte(`{}`),
		Status:           "executing",
		ExpiresAt:        time.Now().UTC().Add(30 * time.Minute),
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewApprovalsHandler(st, nil, nil, nil, config.Config{}, taskrisk.NoopAssessor{}, logger, nil)

	h.processExpiredApproval(ctx, pa, "stranded", "⏰ <b>Recovered</b> — execution lease expired.")

	if strings.Contains(logBuf.String(), "illegal canonical approval transition") {
		t.Fatalf("stranded recovery sweep must not log illegal-transition error\nlogs:\n%s", logBuf.String())
	}

	got, err := st.GetApprovalRecord(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if got.Status != "approved" {
		t.Fatalf("canonical record status after stranded recovery = %q, want %q (user's approval must stand)", got.Status, "approved")
	}
	if got.Resolution != "allow_once" {
		t.Fatalf("canonical record resolution after stranded recovery = %q, want %q", got.Resolution, "allow_once")
	}
}
