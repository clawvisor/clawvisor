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

func TestAuditHandlerNormalizesRuntimeToolUseURLSummary(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "audit-tool-summary.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "audit-tool@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	entry := &store.AuditEntry{
		ID:        "audit-tool-1",
		UserID:    user.ID,
		RequestID: "req-tool-1",
		Timestamp: time.Now().UTC(),
		Service:   "runtime.tool_use",
		Action:    "web_fetch",
		ParamsSafe: json.RawMessage(`{
			"tool_name":"web_fetch",
			"tool_input":{"url":"https://example.com","maxChars":8000}
		}`),
		Decision:   "allow",
		Outcome:    "executed",
		DurationMS: 33,
	}
	if err := st.LogAudit(ctx, entry); err != nil {
		t.Fatalf("LogAudit: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/audit/"+entry.ID, nil)
	req.SetPathValue("id", entry.ID)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	h := NewAuditHandler(st)
	h.Get(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Get status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		SummaryText  string `json:"summary_text"`
		ActivityKind string `json:"activity_kind"`
		ActionTarget string `json:"action_target"`
		ToolName     string `json:"tool_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.ActivityKind != "runtime_tool_use" {
		t.Fatalf("unexpected activity kind: %+v", resp)
	}
	if resp.ToolName != "web_fetch" {
		t.Fatalf("unexpected tool name: %+v", resp)
	}
	if resp.ActionTarget != "https://example.com" {
		t.Fatalf("unexpected action target: %+v", resp)
	}
	if resp.SummaryText != "web_fetch https://example.com" {
		t.Fatalf("unexpected summary: %+v", resp)
	}
}

func TestLooksSecretKeyTokenBoundary(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		// Should match — direct credential keys.
		{"token", true},
		{"access_token", true},
		{"refresh_token", true},
		{"oauth_token", true},
		{"Authorization", true},
		{"authorization", true},
		{"auth", true},
		{"auth_header", true},
		{"api_key", true},
		{"apiKey", true},
		{"x-api-key", true},
		{"access_key", true},
		{"private_key", true},
		{"privateKey", true},
		{"client_secret", true},
		{"password", true},
		{"bearer", true},

		// Versioned key names — must split on letter↔digit boundaries so
		// the semantic word still surfaces to the allowlist.
		{"oauth2Token", true},
		{"oauth2_token", true},
		{"v2AccessToken", true},
		{"oauth2_password", true},

		// Should NOT match — historical false positives from substring matcher.
		{"oauth_endpoint", false},
		{"oauth_url", false},
		{"oauth_state", false},
		{"author", false},
		{"authority", false},
		{"authentication_method", false}, // method name, not secret
		{"keypath", false},
		{"keyboard", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if got := looksSecretKey(tc.key); got != tc.want {
				t.Fatalf("looksSecretKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestAuditHandlerStripsSecretsFromLegacyParamsSafeAtReadTime(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "audit-redaction.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "audit-redaction@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	entry := &store.AuditEntry{
		ID:        "audit-redact-1",
		UserID:    user.ID,
		RequestID: "req-redact-1",
		Timestamp: time.Now().UTC(),
		Service:   "runtime.egress",
		Action:    "post",
		ParamsSafe: json.RawMessage(`{
			"method":"POST",
			"host":"api.example.com",
			"path":"/v1/messages",
			"headers":{"authorization":"Bearer sk-secret-token"}
		}`),
		Decision:   "allow",
		Outcome:    "executed",
		DurationMS: 12,
	}
	if err := st.LogAudit(ctx, entry); err != nil {
		t.Fatalf("LogAudit: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/audit/"+entry.ID, nil)
	req.SetPathValue("id", entry.ID)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	NewAuditHandler(st).Get(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Get status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ParamsSafe map[string]any `json:"params_safe"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	headers, _ := resp.ParamsSafe["headers"].(map[string]any)
	if _, ok := headers["authorization"]; ok {
		t.Fatalf("expected authorization header to be removed, got %+v", headers)
	}
}
