package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestAuditHandlerListExcludesMutedRuntimeEgressRows(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "audit-mutes.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "audit-mutes@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := st.CreateActivityMute(ctx, &store.ActivityMute{
		UserID:     user.ID,
		Host:       "127.0.0.1",
		PathPrefix: "/healthz",
	}); err != nil {
		t.Fatalf("CreateActivityMute: %v", err)
	}

	if err := st.LogAudit(ctx, &store.AuditEntry{
		UserID:     user.ID,
		RequestID:  "req-muted",
		Timestamp:  time.Now().UTC(),
		Service:    "runtime.egress",
		Action:     "get",
		ParamsSafe: json.RawMessage(`{"host":"127.0.0.1","path":"/healthz","headers":{}}`),
		Decision:   "allow",
		Outcome:    "executed",
		DurationMS: 12,
	}); err != nil {
		t.Fatalf("LogAudit(muted): %v", err)
	}

	if err := st.LogAudit(ctx, &store.AuditEntry{
		UserID:     user.ID,
		RequestID:  "req-visible",
		Timestamp:  time.Now().UTC().Add(1 * time.Second),
		Service:    "runtime.egress",
		Action:     "get",
		ParamsSafe: json.RawMessage(`{"host":"127.0.0.1","path":"/metrics","headers":{}}`),
		Decision:   "allow",
		Outcome:    "executed",
		DurationMS: 8,
	}); err != nil {
		t.Fatalf("LogAudit(visible): %v", err)
	}

	if err := st.LogAudit(ctx, &store.AuditEntry{
		UserID:     user.ID,
		RequestID:  "req-service",
		Timestamp:  time.Now().UTC().Add(2 * time.Second),
		Service:    "google.gmail",
		Action:     "list_messages",
		ParamsSafe: json.RawMessage(`{"label":"inbox"}`),
		Decision:   "execute",
		Outcome:    "executed",
		DurationMS: 25,
	}); err != nil {
		t.Fatalf("LogAudit(service): %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h := NewAuditHandler(st)
	h.List(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("List status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Entries []store.AuditEntry `json:"entries"`
		Total   int                `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Total != 2 || len(resp.Entries) != 2 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	for _, entry := range resp.Entries {
		if entry.RequestID == "req-muted" {
			t.Fatalf("muted runtime egress row should not be returned: %+v", entry)
		}
	}
}
