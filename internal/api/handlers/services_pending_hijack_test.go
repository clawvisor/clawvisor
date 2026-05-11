package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlitestore "github.com/clawvisor/clawvisor/pkg/store/sqlite"
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

	got, err := st.GetPendingApproval(ctx, "req-victim-1", victim.ID)
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

// TestCheckPendingRequestOwnership_RejectsCrossUser exercises the upstream
// guard used by every OAuth init path (POST /api/oauth/init, GET /api/oauth/url,
// GET /api/oauth/start). The downstream reactivatePendingRequest check is
// separately covered above; this guard makes sure attacker-controlled IDs
// never get stashed in OAuth state in the first place.
func TestCheckPendingRequestOwnership_RejectsCrossUser(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := sqlitestore.NewStore(db)
	victim, _ := st.CreateUser(ctx, "v@test", "h")
	attacker, _ := st.CreateUser(ctx, "a@test", "h")

	pa := &store.PendingApproval{
		ID:        "pa-x",
		UserID:    victim.ID,
		RequestID: "req-x",
		AuditID:   "audit-x",
		Status:    "pending",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if err := st.SavePendingApproval(ctx, pa); err != nil {
		t.Fatalf("SavePendingApproval: %v", err)
	}

	h := NewServicesHandler(st, nil, adapters.NewRegistry(),
		slog.New(slog.NewTextHandler(discardWriter{}, nil)), "", nil)

	cases := []struct {
		name       string
		callerID   string
		pendingID  string
		wantOK     bool
		wantStatus int
	}{
		{"empty pending id is allowed", attacker.ID, "", true, 0},
		{"victim's id rejected for attacker", attacker.ID, "req-x", false, http.StatusForbidden},
		{"unknown id rejected", attacker.ID, "does-not-exist", false, http.StatusForbidden},
		{"victim's id allowed for victim", victim.ID, "req-x", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/oauth/start", nil)
			ok := h.checkPendingRequestOwnership(rec, req, tc.callerID, tc.pendingID)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want=%v", ok, tc.wantOK)
			}
			if !tc.wantOK && rec.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d", rec.Code, tc.wantStatus)
			}
		})
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
