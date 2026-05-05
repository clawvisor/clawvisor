package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	sqlitestore "github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestReactivatePendingRequest_RejectsCrossUserHijack verifies that
// reactivatePendingRequest refuses to act on a pending approval that belongs
// to a different user than the one whose OAuth flow just completed.
//
// The vulnerability: services.go previously fetched the pending approval by
// request_id alone, then executed against the OAuth user's credentials.
// An attacker could supply victim's pending_request_id during their own
// OAuth init; on callback, the victim's pending row would be deleted and
// the victim's audit trail mutated — even though the actual execution would
// run with the attacker's vault credential.
func TestReactivatePendingRequest_RejectsCrossUserHijack(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := sqlitestore.NewStore(db)
	victim, err := st.CreateUser(ctx, "victim@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser victim: %v", err)
	}
	attacker, err := st.CreateUser(ctx, "attacker@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser attacker: %v", err)
	}

	// Create a pending approval owned by the victim.
	blob, _ := json.Marshal(map[string]any{
		"service": "mock.echo",
		"action":  "echo",
		"params":  map[string]any{"msg": "victim secret"},
		"user_id": victim.ID,
	})
	pa := &store.PendingApproval{
		ID:          "pa-victim-1",
		UserID:      victim.ID,
		RequestID:   "req-victim-1",
		AuditID:     "audit-victim-1",
		RequestBlob: blob,
		Status:      "pending",
		ExpiresAt:   time.Now().Add(5 * time.Minute),
	}
	// Audit row referenced by AuditID must exist for UpdateAuditOutcome to be
	// observable; we don't insert one because we want to *prove* nothing gets
	// touched.
	if err := st.SavePendingApproval(ctx, pa); err != nil {
		t.Fatalf("SavePendingApproval: %v", err)
	}

	h := NewServicesHandler(st, nil, adapters.NewRegistry(),
		slog.New(slog.NewTextHandler(discardWriter{}, nil)), "", nil)

	// Attacker just completed OAuth; reactivate is invoked with attacker.ID
	// but victim's request_id. Must be a no-op.
	h.reactivatePendingRequest(ctx, attacker.ID, "req-victim-1")

	got, err := st.GetPendingApproval(ctx, "req-victim-1")
	if err != nil {
		t.Fatalf("victim's pending approval was deleted by cross-user reactivation: %v", err)
	}
	if got.UserID != victim.ID {
		t.Fatalf("victim's pending approval mutated: user_id=%q want %q", got.UserID, victim.ID)
	}
	if got.Status != "pending" {
		t.Fatalf("victim's pending approval status changed to %q", got.Status)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
