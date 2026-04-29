package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	runtimeproxy "github.com/clawvisor/clawvisor/internal/runtime/proxy"
	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/internal/vault"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestRuntimeHandlerCreatePlaceholder(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-placeholder.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}

	user, err := st.CreateUser(ctx, "runtime-placeholder@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := v.Set(ctx, user.ID, "google.gmail:work", []byte(`{"access_token":"real-token"}`)); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"service": "google.gmail:work"})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/placeholders", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(store.WithAgent(req.Context(), agent))

	rec := httptest.NewRecorder()
	h := NewRuntimeHandler(st, v, nil, nil)
	h.CreatePlaceholder(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("CreatePlaceholder status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	placeholder, _ := resp["placeholder"].(string)
	if placeholder == "" {
		t.Fatal("expected placeholder in response")
	}
	meta, err := st.GetRuntimePlaceholder(ctx, placeholder)
	if err != nil {
		t.Fatalf("GetRuntimePlaceholder: %v", err)
	}
	if meta.AgentID != agent.ID || meta.UserID != user.ID || meta.ServiceID != "google.gmail:work" {
		t.Fatalf("unexpected placeholder metadata: %+v", meta)
	}
}

func TestRuntimeHandlerOneOffTTLDefaultsWhenConfigNil(t *testing.T) {
	h := NewRuntimeHandler(nil, nil, nil, nil)
	if got := h.oneOffTTLSeconds(); got != 300 {
		t.Fatalf("oneOffTTLSeconds()=%d, want 300", got)
	}
}

func TestRuntimeHandlerListEvents(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-events.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "runtime-events@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	session := &store.RuntimeSession{
		ID:                    "runtime-events-session",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "secret-hash",
		ExpiresAt:             time.Now().UTC().Add(5 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if err := st.CreateRuntimeEvent(ctx, &store.RuntimeEvent{
		SessionID: session.ID,
		UserID:    user.ID,
		AgentID:   agent.ID,
		EventType: "runtime.egress.allowed",
	}); err != nil {
		t.Fatalf("CreateRuntimeEvent: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/runtime/events?session_id="+session.ID, nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h := NewRuntimeHandler(st, nil, nil, nil)
	h.ListEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ListEvents status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Entries []store.RuntimeEvent `json:"entries"`
		Total   int                  `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Total != 1 || len(resp.Entries) != 1 || resp.Entries[0].EventType != "runtime.egress.allowed" {
		t.Fatalf("unexpected events response: %+v", resp)
	}
}

func TestRuntimeHandlerResolveApprovalCreatesOneOffEvent(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-approval-events.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "runtime-approval@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	session := &store.RuntimeSession{
		ID:                    "runtime-approval-session",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "secret-hash",
		ExpiresAt:             time.Now().UTC().Add(5 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	payload, _ := json.Marshal(runtimeproxy.RuntimeApprovalPayload{
		SessionID:          session.ID,
		AgentID:            agent.ID,
		RequestFingerprint: "fp-1",
		Method:             "GET",
		Host:               "example.com",
		Path:               "/blocked",
	})
	approval := &store.ApprovalRecord{
		ID:                  "approval-runtime-oneoff",
		Kind:                "request_once",
		UserID:              user.ID,
		AgentID:             &agent.ID,
		SessionID:           &session.ID,
		Status:              "pending",
		Surface:             "dashboard",
		PayloadJSON:         payload,
		ResolutionTransport: "consume_one_off_retry",
	}
	if err := st.CreateApprovalRecord(ctx, approval); err != nil {
		t.Fatalf("CreateApprovalRecord: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"resolution": "allow_once"})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/approvals/"+approval.ID+"/resolve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", approval.ID)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h := NewRuntimeHandler(st, nil, nil, nil)
	h.ResolveApproval(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ResolveApproval status=%d body=%s", rec.Code, rec.Body.String())
	}
	events, err := st.ListRuntimeEvents(ctx, user.ID, store.RuntimeEventFilter{SessionID: session.ID, Limit: 10})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	if len(events) == 0 || events[0].EventType != "runtime.egress.one_off_created" {
		t.Fatalf("expected one_off_created runtime event, got %+v", events)
	}
}
