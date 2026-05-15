package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/config"
	runtimeproxy "github.com/clawvisor/clawvisor/pkg/runtime/proxy"
	runtimereview "github.com/clawvisor/clawvisor/pkg/runtime/review"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/pkg/vault"
	"github.com/google/uuid"
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
	h := NewRuntimeHandler(st, v, nil, nil, nil)
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

func TestRuntimeHandlerCreateUserPlaceholderFromVaultItem(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-user-placeholder.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}

	user, err := st.CreateUser(ctx, "runtime-user-placeholder@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := v.Set(ctx, user.ID, "anthropic", []byte(`sk-ant-test-key`)); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"service":     "llm:anthropic:user",
		"ttl_seconds": 900,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/placeholders/mint", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))

	before := time.Now().UTC()
	rec := httptest.NewRecorder()
	h := NewRuntimeHandler(st, v, nil, nil, nil)
	h.CreateUserPlaceholder(rec, req)
	after := time.Now().UTC()

	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateUserPlaceholder status=%d body=%s", rec.Code, rec.Body.String())
	}
	var entry store.RuntimePlaceholder
	if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if entry.Placeholder == "" {
		t.Fatal("expected placeholder in response")
	}
	if entry.AgentID != "" {
		t.Fatalf("manual placeholder should be user-wide when agent_id is omitted, got %q", entry.AgentID)
	}
	if entry.ServiceID != "llm:anthropic:user" || entry.VaultItemID != "llm:anthropic:user" {
		t.Fatalf("placeholder should preserve requested vault item id, got %+v", entry)
	}
	if entry.CredentialGrantID == "" {
		t.Fatalf("expected credential grant on manual placeholder: %+v", entry)
	}
	if entry.ExpiresAt == nil {
		t.Fatalf("expected expiration on manual placeholder: %+v", entry)
	}
	if entry.ExpiresAt.Before(before.Add(899*time.Second)) || entry.ExpiresAt.After(after.Add(901*time.Second)) {
		t.Fatalf("unexpected expiration %s", entry.ExpiresAt)
	}
	auth, err := st.GetCredentialAuthorization(ctx, entry.CredentialGrantID)
	if err != nil {
		t.Fatalf("GetCredentialAuthorization: %v", err)
	}
	if auth.AgentID != "" || auth.Scope != "manual" || auth.CredentialRef != "anthropic" || auth.Service != "llm:anthropic:user" {
		t.Fatalf("unexpected credential grant: %+v", auth)
	}
	if auth.ExpiresAt == nil || auth.ExpiresAt.Sub(*entry.ExpiresAt) > time.Second || entry.ExpiresAt.Sub(*auth.ExpiresAt) > time.Second {
		t.Fatalf("credential grant should share placeholder expiration, auth=%+v placeholder=%+v", auth, entry)
	}

	defaultTTLBody, _ := json.Marshal(map[string]any{"service": "llm:anthropic:user"})
	defaultTTLReq := httptest.NewRequest(http.MethodPost, "/api/runtime/placeholders/mint", bytes.NewReader(defaultTTLBody))
	defaultTTLReq.Header.Set("Content-Type", "application/json")
	defaultTTLReq = defaultTTLReq.WithContext(context.WithValue(defaultTTLReq.Context(), middleware.UserContextKey, user))
	defaultBefore := time.Now().UTC()
	defaultRec := httptest.NewRecorder()
	h.CreateUserPlaceholder(defaultRec, defaultTTLReq)
	defaultAfter := time.Now().UTC()
	if defaultRec.Code != http.StatusCreated {
		t.Fatalf("CreateUserPlaceholder default TTL status=%d body=%s", defaultRec.Code, defaultRec.Body.String())
	}
	var defaultEntry store.RuntimePlaceholder
	if err := json.Unmarshal(defaultRec.Body.Bytes(), &defaultEntry); err != nil {
		t.Fatalf("unmarshal default TTL response: %v", err)
	}
	if defaultEntry.ExpiresAt == nil {
		t.Fatalf("expected default expiration on manual placeholder: %+v", defaultEntry)
	}
	if defaultEntry.ExpiresAt.Before(defaultBefore.Add(time.Hour-time.Second)) || defaultEntry.ExpiresAt.After(defaultAfter.Add(time.Hour+time.Second)) {
		t.Fatalf("unexpected default expiration %s", defaultEntry.ExpiresAt)
	}
}

func TestRuntimeHandlerOneOffTTLDefaultsWhenConfigNil(t *testing.T) {
	h := NewRuntimeHandler(nil, nil, nil, nil, nil)
	if got := h.oneOffTTLSeconds(); got != 300 {
		t.Fatalf("oneOffTTLSeconds()=%d, want 300", got)
	}
}

func TestRuntimeHandlerStatusAndSessionsWithoutRuntimeManager(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-lite.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "runtime-lite@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = false
	cfg.ProxyLite.Enabled = true

	var runtimeMgr *runtimeproxy.Manager
	h := NewRuntimeHandler(st, nil, runtimeMgr, cfg, nil)

	statusReq := httptest.NewRequest(http.MethodGet, "/api/runtime/status", nil)
	statusReq = statusReq.WithContext(context.WithValue(statusReq.Context(), middleware.UserContextKey, user))
	statusRec := httptest.NewRecorder()
	h.Status(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("Status status=%d body=%s", statusRec.Code, statusRec.Body.String())
	}
	var statusResp struct {
		Enabled          bool   `json:"enabled"`
		ProxyLiteEnabled bool   `json:"proxy_lite_enabled"`
		ProxyURL         string `json:"proxy_url"`
		CACertPEM        string `json:"ca_cert_pem"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if statusResp.Enabled {
		t.Fatalf("full runtime proxy should be disabled: %+v", statusResp)
	}
	if !statusResp.ProxyLiteEnabled {
		t.Fatalf("proxy-lite should be reported enabled: %+v", statusResp)
	}
	if statusResp.ProxyURL != "" || statusResp.CACertPEM != "" {
		t.Fatalf("nil manager should not report full-proxy connection data: %+v", statusResp)
	}

	sessionsReq := httptest.NewRequest(http.MethodGet, "/api/runtime/sessions", nil)
	sessionsReq = sessionsReq.WithContext(context.WithValue(sessionsReq.Context(), middleware.UserContextKey, user))
	sessionsRec := httptest.NewRecorder()
	h.ListSessions(sessionsRec, sessionsReq)
	if sessionsRec.Code != http.StatusOK {
		t.Fatalf("ListSessions status=%d body=%s", sessionsRec.Code, sessionsRec.Body.String())
	}
	var sessionsResp struct {
		Entries []store.RuntimeSession `json:"entries"`
		Total   int                    `json:"total"`
	}
	if err := json.Unmarshal(sessionsRec.Body.Bytes(), &sessionsResp); err != nil {
		t.Fatalf("unmarshal sessions: %v", err)
	}
	if sessionsResp.Total != 0 || len(sessionsResp.Entries) != 0 {
		t.Fatalf("expected empty sessions without runtime manager: %+v", sessionsResp)
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
	h := NewRuntimeHandler(st, nil, nil, nil, nil)
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

func TestRuntimeHandlerListApprovalsExcludesRevokedAndExpiredSessions(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-approvals-list.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "runtime-approvals-list@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	liveSession := &store.RuntimeSession{
		ID:                    "runtime-live-session",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "live-secret-hash",
		ExpiresAt:             time.Now().UTC().Add(5 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, liveSession); err != nil {
		t.Fatalf("CreateRuntimeSession(live): %v", err)
	}

	revokedSession := &store.RuntimeSession{
		ID:                    "runtime-revoked-session",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "revoked-secret-hash",
		ExpiresAt:             time.Now().UTC().Add(5 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, revokedSession); err != nil {
		t.Fatalf("CreateRuntimeSession(revoked): %v", err)
	}
	if err := st.RevokeRuntimeSession(ctx, revokedSession.ID, time.Now().UTC()); err != nil {
		t.Fatalf("RevokeRuntimeSession: %v", err)
	}

	expiredSession := &store.RuntimeSession{
		ID:                    "runtime-expired-session",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "expired-secret-hash",
		ExpiresAt:             time.Now().UTC().Add(5 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, expiredSession); err != nil {
		t.Fatalf("CreateRuntimeSession(expired): %v", err)
	}
	if err := st.UpdateRuntimeSessionExpiry(ctx, expiredSession.ID, time.Now().UTC().Add(-5*time.Minute)); err != nil {
		t.Fatalf("UpdateRuntimeSessionExpiry: %v", err)
	}

	createApproval := func(id, sessionID string) {
		payload, _ := json.Marshal(runtimeproxy.RuntimeApprovalPayload{
			SessionID:          sessionID,
			AgentID:            agent.ID,
			RequestFingerprint: id,
			Method:             "GET",
			Host:               "example.com",
			Path:               "/healthz",
		})
		rec := &store.ApprovalRecord{
			ID:                  id,
			Kind:                "request_once",
			UserID:              user.ID,
			AgentID:             &agent.ID,
			SessionID:           &sessionID,
			Status:              "pending",
			Surface:             "dashboard",
			PayloadJSON:         payload,
			ResolutionTransport: "consume_one_off_retry",
		}
		if err := st.CreateApprovalRecord(ctx, rec); err != nil {
			t.Fatalf("CreateApprovalRecord(%s): %v", id, err)
		}
	}

	createApproval("approval-live", liveSession.ID)
	createApproval("approval-revoked", revokedSession.ID)
	createApproval("approval-expired", expiredSession.ID)

	req := httptest.NewRequest(http.MethodGet, "/api/runtime/approvals", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h := NewRuntimeHandler(st, nil, nil, nil, nil)
	h.ListApprovals(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ListApprovals status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Entries []store.ApprovalRecord `json:"entries"`
		Total   int                    `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Total != 1 || len(resp.Entries) != 1 || resp.Entries[0].ID != "approval-live" {
		t.Fatalf("unexpected approvals response: %+v", resp)
	}

	revokedApproval, err := st.GetApprovalRecord(ctx, "approval-revoked")
	if err != nil {
		t.Fatalf("GetApprovalRecord(approval-revoked): %v", err)
	}
	if revokedApproval.Status != "pending" || revokedApproval.Resolution != "" || revokedApproval.ResolvedAt != nil {
		t.Fatalf("expected revoked-session approval to remain unresolved when filtered from list: %+v", revokedApproval)
	}

	expiredApproval, err := st.GetApprovalRecord(ctx, "approval-expired")
	if err != nil {
		t.Fatalf("GetApprovalRecord(approval-expired): %v", err)
	}
	if expiredApproval.Status != "pending" || expiredApproval.Resolution != "" || expiredApproval.ResolvedAt != nil {
		t.Fatalf("expected expired-session approval to remain unresolved when filtered from list: %+v", expiredApproval)
	}
}

// Regression: task_create and task_expand approvals already surface in
// the dedicated Tasks UI. Including them in the runtime-approvals
// queue too produced a confusing "duplicate" badge (the same task
// appearing as both a Task row AND a "runtime retry approval" item).
// They must be filtered out of ListApprovals.
func TestRuntimeHandlerListApprovalsExcludesTaskApprovals(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-approvals-task-filter.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "task-approvals-filter@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "task-filter-agent", "task-filter-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	task := &store.Task{
		ID:      "task-1",
		UserID:  user.ID,
		AgentID: agent.ID,
		Status:  "pending",
		Purpose: "Filter test",
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	taskApproval := &store.ApprovalRecord{
		ID:                  "approval-task-create",
		Kind:                "task_create",
		UserID:              user.ID,
		AgentID:             &agent.ID,
		TaskID:              &task.ID,
		Status:              "pending",
		Surface:             "dashboard",
		ResolutionTransport: "task_state_update",
	}
	if err := st.CreateApprovalRecord(ctx, taskApproval); err != nil {
		t.Fatalf("CreateApprovalRecord(task_create): %v", err)
	}

	// A normal runtime approval that DOES belong in the list.
	payload, _ := json.Marshal(runtimeproxy.RuntimeApprovalPayload{
		AgentID:            agent.ID,
		RequestFingerprint: "request-once-1",
		Method:             "GET",
		Host:               "example.com",
		Path:               "/x",
	})
	requestApproval := &store.ApprovalRecord{
		ID:                  "approval-request-once",
		Kind:                "request_once",
		UserID:              user.ID,
		AgentID:             &agent.ID,
		Status:              "pending",
		Surface:             "dashboard",
		PayloadJSON:         payload,
		ResolutionTransport: "consume_one_off_retry",
	}
	if err := st.CreateApprovalRecord(ctx, requestApproval); err != nil {
		t.Fatalf("CreateApprovalRecord(request_once): %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/runtime/approvals", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h := NewRuntimeHandler(st, nil, nil, nil, nil)
	h.ListApprovals(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ListApprovals status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Entries []store.ApprovalRecord `json:"entries"`
		Total   int                    `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Total != 1 || len(resp.Entries) != 1 || resp.Entries[0].ID != "approval-request-once" {
		t.Fatalf("expected only the non-task approval to appear; got %+v", resp)
	}

	// The task approval must still exist in the store (resolution
	// machinery looks it up by task_id later).
	got, err := st.GetApprovalRecord(ctx, "approval-task-create")
	if err != nil {
		t.Fatalf("task approval should remain in store: %v", err)
	}
	if got.Status != "pending" {
		t.Errorf("task approval status changed unexpectedly: %s", got.Status)
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
	h := NewRuntimeHandler(st, nil, nil, nil, nil)
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

func TestRuntimeHandlerResolveApprovalAllowSessionPromotesRuntimeEgressToTask(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-allow-session.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "runtime-allow-session@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	session := &store.RuntimeSession{
		ID:                    "runtime-promote-session",
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
		RequestFingerprint: "fp-session",
		Method:             "POST",
		Host:               "api.example.com",
		Path:               "/tickets",
		Reason:             "Create support ticket for this run",
		Body:               map[string]any{"title": "printer issue", "priority": "high"},
	})
	approval := &store.ApprovalRecord{
		ID:                  "approval-runtime-session",
		Kind:                "task_create",
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

	body, _ := json.Marshal(map[string]any{"resolution": "allow_session"})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/approvals/"+approval.ID+"/resolve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", approval.ID)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	h.ResolveApproval(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ResolveApproval status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	taskID, _ := resp["task_id"].(string)
	if taskID == "" {
		t.Fatal("expected promoted task_id in response")
	}
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Lifetime != "session" || task.Status != "active" || task.ExpiresAt == nil {
		t.Fatalf("unexpected promoted task: %+v", task)
	}
	if len(task.ExpectedEgress) == 0 {
		t.Fatalf("expected egress envelope on promoted task: %+v", task)
	}
	activeBinding, err := st.GetActiveTaskSession(ctx, task.ID, session.ID)
	if err != nil {
		t.Fatalf("GetActiveTaskSession: %v", err)
	}
	if activeBinding.Status != "active" {
		t.Fatalf("unexpected active binding: %+v", activeBinding)
	}
	events, err := st.ListRuntimeEvents(ctx, user.ID, store.RuntimeEventFilter{SessionID: session.ID, Limit: 10})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	found := false
	for _, event := range events {
		if event.EventType == "runtime.task.promoted" && event.TaskID != nil && *event.TaskID == task.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected runtime.task.promoted event, got %+v", events)
	}
}

func TestRuntimeHandlerResolveApprovalAllowAlwaysPromotesHeldToolReviewAndRebindsCache(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-held-promote.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "runtime-held-promote@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	session := &store.RuntimeSession{
		ID:                    "runtime-held-session",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "secret-hash",
		ExpiresAt:             time.Now().UTC().Add(5 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	payload, _ := json.Marshal(runtimeproxy.HeldToolUseApprovalPayload{
		SessionID: session.ID,
		AgentID:   agent.ID,
		ToolUseID: "toolu_123",
		ToolName:  "fetch_messages",
		ToolInput: map[string]any{"max_results": 10, "label": "inbox"},
		Reason:    "Read inbox contents for this workflow",
	})
	approval := &store.ApprovalRecord{
		ID:                  "approval-held-standing",
		Kind:                "task_create",
		UserID:              user.ID,
		AgentID:             &agent.ID,
		SessionID:           &session.ID,
		Status:              "pending",
		Surface:             "dashboard",
		PayloadJSON:         payload,
		ResolutionTransport: "release_held_tool_use",
	}
	if err := st.CreateApprovalRecord(ctx, approval); err != nil {
		t.Fatalf("CreateApprovalRecord: %v", err)
	}
	reviewCache := runtimereview.NewApprovalCache()
	held, created := reviewCache.Hold(session.ID, approval.ID, "", "toolu_123", "fetch_messages", map[string]any{"max_results": 10, "label": "inbox"}, "Read inbox contents for this workflow")
	if !created || held == nil {
		t.Fatalf("expected held approval in review cache, got created=%v held=%v", created, held)
	}

	body, _ := json.Marshal(map[string]any{"resolution": "allow_always"})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/approvals/"+approval.ID+"/resolve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", approval.ID)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h := NewRuntimeHandler(st, nil, nil, config.Default(), reviewCache)
	h.ResolveApproval(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ResolveApproval status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	taskID, _ := resp["task_id"].(string)
	if taskID == "" {
		t.Fatal("expected promoted task_id in response")
	}
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Lifetime != "standing" || len(task.ExpectedTools) == 0 {
		t.Fatalf("unexpected standing task: %+v", task)
	}
	rebound := reviewCache.GetByApprovalRecord(session.ID, approval.ID)
	if rebound == nil || rebound.TaskID != task.ID {
		t.Fatalf("expected held approval to rebind to standing task, got %+v", rebound)
	}
}

func TestRuntimeHandlerResolveApprovalAllowOnceCreatesCredentialAuthorization(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-credential-once.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "runtime-credential-once@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	session := &store.RuntimeSession{
		ID:                    "runtime-credential-once-session",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "secret-hash",
		ExpiresAt:             time.Now().UTC().Add(5 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	payload, _ := json.Marshal(runtimeproxy.RuntimeCredentialReviewPayload{
		SessionID:     session.ID,
		AgentID:       agent.ID,
		CredentialRef: "sha256:cred-1",
		Service:       "github",
		Host:          "api.github.com",
		HeaderName:    "Authorization",
		Scheme:        "bearer",
		Detector:      "known_service",
	})
	approval := &store.ApprovalRecord{
		ID:                  "approval-credential-once",
		Kind:                "credential_review",
		UserID:              user.ID,
		AgentID:             &agent.ID,
		SessionID:           &session.ID,
		Status:              "pending",
		Surface:             "dashboard",
		PayloadJSON:         payload,
		ResolutionTransport: "create_credential_authorization",
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
	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	h.ResolveApproval(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ResolveApproval status=%d body=%s", rec.Code, rec.Body.String())
	}
	auth, err := st.GetCredentialAuthorization(ctx, uuid.NewSHA1(uuid.NameSpaceURL, []byte("credential-approval:"+approval.ID+":once")).String())
	if err != nil {
		t.Fatalf("GetCredentialAuthorization: %v", err)
	}
	if auth.Scope != "once" || auth.SessionID == nil || *auth.SessionID != session.ID || auth.ExpiresAt == nil {
		t.Fatalf("unexpected once credential authorization: %+v", auth)
	}
}

func TestRuntimeHandlerResolveApprovalAllowAlwaysCreatesStandingCredentialAuthorization(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-credential-standing.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "runtime-credential-standing@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	session := &store.RuntimeSession{
		ID:                    "runtime-credential-standing-session",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "secret-hash",
		ExpiresAt:             time.Now().UTC().Add(5 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	payload, _ := json.Marshal(runtimeproxy.RuntimeCredentialReviewPayload{
		SessionID:     session.ID,
		AgentID:       agent.ID,
		CredentialRef: "sha256:cred-2",
		Service:       "github",
		Host:          "api.github.com",
		HeaderName:    "Authorization",
		Scheme:        "bearer",
		Detector:      "known_service",
	})
	approval := &store.ApprovalRecord{
		ID:                  "approval-credential-standing",
		Kind:                "credential_review",
		UserID:              user.ID,
		AgentID:             &agent.ID,
		SessionID:           &session.ID,
		Status:              "pending",
		Surface:             "dashboard",
		PayloadJSON:         payload,
		ResolutionTransport: "create_credential_authorization",
	}
	if err := st.CreateApprovalRecord(ctx, approval); err != nil {
		t.Fatalf("CreateApprovalRecord: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"resolution": "allow_always"})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/approvals/"+approval.ID+"/resolve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", approval.ID)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	h.ResolveApproval(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ResolveApproval status=%d body=%s", rec.Code, rec.Body.String())
	}
	auth, err := st.GetCredentialAuthorization(ctx, uuid.NewSHA1(uuid.NameSpaceURL, []byte("credential-approval:"+approval.ID+":standing")).String())
	if err != nil {
		t.Fatalf("GetCredentialAuthorization: %v", err)
	}
	if auth.Scope != "standing" || auth.SessionID != nil || auth.CredentialRef != "sha256:cred-2" {
		t.Fatalf("unexpected standing credential authorization: %+v", auth)
	}
	events, err := st.ListRuntimeEvents(ctx, user.ID, store.RuntimeEventFilter{SessionID: session.ID, Limit: 10})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	found := false
	for _, event := range events {
		if event.EventType == "runtime.autovault.authorization_created" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected runtime.autovault.authorization_created event, got %+v", events)
	}
}

func TestRuntimeHandlerResolveApprovalRejectsIllegalTransition(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-approval-illegal-transition.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "runtime-illegal-transition@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	session := &store.RuntimeSession{
		ID:                    "runtime-illegal-transition-session",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "secret-hash",
		ExpiresAt:             time.Now().UTC().Add(5 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	payload, _ := json.Marshal(runtimeproxy.HeldToolUseApprovalPayload{
		SessionID: session.ID,
		AgentID:   agent.ID,
		ToolUseID: "toolu_illegal_transition",
		ToolName:  "Bash",
		ToolInput: map[string]any{"command": "touch /tmp/example"},
	})
	approval := &store.ApprovalRecord{
		ID:                  "approval-illegal-transition",
		Kind:                "task_create",
		UserID:              user.ID,
		AgentID:             &agent.ID,
		SessionID:           &session.ID,
		Status:              "pending",
		Surface:             "dashboard",
		PayloadJSON:         payload,
		ResolutionTransport: "release_held_tool_use",
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
	h := NewRuntimeHandler(st, nil, nil, config.Default(), nil)
	h.ResolveApproval(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("ResolveApproval status=%d body=%s", rec.Code, rec.Body.String())
	}
	resolved, err := st.GetApprovalRecord(ctx, approval.ID)
	if err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if resolved.Status != "pending" || resolved.Resolution != "" {
		t.Fatalf("expected approval to remain pending after rejected transition, got %+v", resolved)
	}
}

type failingCredentialAuthorizationStore struct {
	store.Store
	err error
}

func (s failingCredentialAuthorizationStore) CreateCredentialAuthorization(ctx context.Context, auth *store.CredentialAuthorization) error {
	return s.err
}

func TestRuntimeHandlerResolveApprovalReturnsErrorWhenCredentialAuthorizationWriteFails(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-credential-write-fail.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	baseStore := sqlite.NewStore(db)

	user, err := baseStore.CreateUser(ctx, "runtime-credential-write-fail@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := baseStore.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	session := &store.RuntimeSession{
		ID:                    "runtime-credential-write-fail-session",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "secret-hash",
		ExpiresAt:             time.Now().UTC().Add(5 * time.Minute),
	}
	if err := baseStore.CreateRuntimeSession(ctx, session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	payload, _ := json.Marshal(runtimeproxy.RuntimeCredentialReviewPayload{
		SessionID:     session.ID,
		AgentID:       agent.ID,
		CredentialRef: "sha256:cred-fail",
		Service:       "github",
		Host:          "api.github.com",
		HeaderName:    "Authorization",
		Scheme:        "bearer",
		Detector:      "known_service",
	})
	approval := &store.ApprovalRecord{
		ID:                  "approval-credential-write-fail",
		Kind:                "credential_review",
		UserID:              user.ID,
		AgentID:             &agent.ID,
		SessionID:           &session.ID,
		Status:              "pending",
		Surface:             "dashboard",
		PayloadJSON:         payload,
		ResolutionTransport: "create_credential_authorization",
	}
	if err := baseStore.CreateApprovalRecord(ctx, approval); err != nil {
		t.Fatalf("CreateApprovalRecord: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"resolution": "allow_once"})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/approvals/"+approval.ID+"/resolve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", approval.ID)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h := NewRuntimeHandler(failingCredentialAuthorizationStore{Store: baseStore, err: errors.New("boom")}, nil, nil, config.Default(), nil)
	h.ResolveApproval(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("ResolveApproval status=%d body=%s", rec.Code, rec.Body.String())
	}
	events, err := baseStore.ListRuntimeEvents(ctx, user.ID, store.RuntimeEventFilter{SessionID: session.ID, Limit: 10})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	for _, event := range events {
		if event.EventType == "runtime.autovault.authorization_created" {
			t.Fatalf("did not expect authorization_created event after write failure, got %+v", events)
		}
	}
}
