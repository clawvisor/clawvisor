package llmproxy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pricing"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func newAuditTestStore(t *testing.T) (store.Store, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "audit@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "audit-agent", "agent-token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return st, agent
}

func TestAuditEmitter_LogEndpointCall(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.LogEndpointCall(context.Background(), agent, "req-1", "anthropic", "lite_proxy.messages.create",
		200, "allow", "success", "", 12*time.Millisecond, map[string]any{"input_tokens": 18, "output_tokens": 8}, EndpointCallExtras{})

	rows, _, err := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	row := rows[0]
	if row.Service != "anthropic" {
		t.Errorf("service=%q, want anthropic", row.Service)
	}
	if row.Action != "lite_proxy.messages.create" {
		t.Errorf("action=%q", row.Action)
	}
	if row.Decision != "allow" || row.Outcome != "success" {
		t.Errorf("decision/outcome mismatch: %s / %s", row.Decision, row.Outcome)
	}
	if row.AgentID == nil || *row.AgentID != agent.ID {
		t.Errorf("agent_id missing or wrong: %v", row.AgentID)
	}
	if row.DurationMS != 12 {
		t.Errorf("duration_ms=%d, want 12", row.DurationMS)
	}
	var params map[string]any
	if err := json.Unmarshal(row.ParamsSafe, &params); err != nil {
		t.Fatalf("params unmarshal: %v", err)
	}
	if params["input_tokens"] != float64(18) {
		t.Errorf("expected input_tokens=18, got %v", params["input_tokens"])
	}
	if params["http_status"] != float64(200) {
		t.Errorf("expected http_status=200, got %v", params["http_status"])
	}
	if _, ok := params["build_sha"]; !ok {
		t.Errorf("expected forensic build_sha")
	}
	if _, ok := params["parser_version"]; !ok {
		t.Errorf("expected forensic parser_version")
	}
}

// TestAuditEmitter_LogEndpointCall_DedupCostUsesCanonicalAuditID
// pins the FK-safety contract on the dedup path: when LogAudit
// returns ErrConflict (the canonical row already exists for this
// request_id), the cost row must be written against the surviving
// canonical audit row's id — not the locally generated id that
// never landed. With FK llm_request_cost.audit_id REFERENCES
// audit_log(id), using the local id would fail the insert.
func TestAuditEmitter_LogEndpointCall_DedupCostUsesCanonicalAuditID(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)
	ctx := context.Background()

	usage := &ExtractUsageResult{
		Found: true,
		Model: "claude-opus-4-7",
		Usage: pricing.Usage{InputTokens: 100, OutputTokens: 50},
	}

	// First call lands the canonical audit row + cost row.
	em.LogEndpointCall(ctx, agent, "req-dedup", "anthropic", "lite_proxy.messages.create",
		200, "allow", "success", "", 0, nil, EndpointCallExtras{Usage: usage})

	rows, _, err := st.ListAuditEntries(ctx, agent.UserID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row after first call, got %d", len(rows))
	}
	canonicalAuditID := rows[0].ID

	costSummary, err := st.GetTaskCost(ctx, agent.UserID, "")
	if err != nil {
		t.Fatalf("GetTaskCost: %v", err)
	}
	_ = costSummary // task_id is NULL on these rows; just confirm no error

	// Second call with same request_id triggers ErrConflict on LogAudit.
	// The cost record must succeed (FK-safe) by pointing at the
	// canonical row's id, not the new entry's local id.
	em.LogEndpointCall(ctx, agent, "req-dedup", "anthropic", "lite_proxy.messages.create",
		200, "allow", "success", "", 0, nil, EndpointCallExtras{Usage: usage})

	// Still exactly one audit row (dedup worked).
	rows, _, err = st.ListAuditEntries(ctx, agent.UserID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries after retry: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row after retry (dedup), got %d", len(rows))
	}
	if rows[0].ID != canonicalAuditID {
		t.Fatalf("canonical audit id changed unexpectedly: %s -> %s", canonicalAuditID, rows[0].ID)
	}

	// The cost row from the FIRST call must still be there (PK conflict
	// on audit_id meant the retry insert was a no-op rather than a row
	// pointing at a non-existent audit_id). If the dedup path had used
	// a fresh entry.ID, the FK would have rejected it; if there were
	// no FK, we'd now have two cost rows for one canonical audit row.
	sqliteStore, ok := st.(*sqlite.Store)
	if !ok {
		t.Fatalf("expected *sqlite.Store, got %T", st)
	}
	db := sqliteStore.DB()
	var costRowCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM llm_request_cost WHERE audit_id = ?`, canonicalAuditID,
	).Scan(&costRowCount); err != nil {
		t.Fatalf("count cost rows: %v", err)
	}
	if costRowCount != 1 {
		t.Fatalf("expected exactly 1 cost row tied to canonical audit_id %s, got %d",
			canonicalAuditID, costRowCount)
	}

	// And no orphan cost rows pointing at audit ids that don't exist.
	var orphans int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM llm_request_cost c
		 LEFT JOIN audit_log a ON a.id = c.audit_id
		 WHERE a.id IS NULL`,
	).Scan(&orphans); err != nil {
		t.Fatalf("count orphan cost rows: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("found %d orphan cost rows pointing at non-existent audit_log rows", orphans)
	}
}

func TestAuditEmitter_LogToolUseInspected(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	verdict := inspector.Verdict{
		IsAPICall:    true,
		Method:       "POST",
		Host:         "api.github.com",
		Path:         "/repos/x/y/issues",
		Source:       inspector.SourceDeterministic,
		Reason:       "structured fetch with header credential",
		Placeholders: []string{"autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
		CredentialLocations: []inspector.CredentialLocation{
			{Kind: "header", Name: "Authorization", Scheme: "Bearer"},
		},
	}
	em.LogToolUseInspected(context.Background(), agent, "req-1", conversation.ToolUse{
		ID:    "toolu_1",
		Name:  "WebFetch",
		Input: json.RawMessage(`{"url":"https://api.github.com/repos/x/y/issues","headers":{"Authorization":"Bearer secret"}}`),
	}, verdict, "rewrite", "success", verdict.Reason, "")

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.Service != "runtime.tool_use" {
		t.Errorf("service=%q, want runtime.tool_use", row.Service)
	}
	if row.Action != "lite_proxy.tool_use.rewrite" {
		t.Errorf("action=%q", row.Action)
	}
	if row.ToolUseID == nil || *row.ToolUseID != "toolu_1" {
		t.Errorf("tool_use_id missing or wrong: %v", row.ToolUseID)
	}
	if row.TaskID != nil {
		t.Errorf("task_id should be nil when caller passes empty string, got %v", row.TaskID)
	}
	var params map[string]any
	_ = json.Unmarshal(row.ParamsSafe, &params)
	if params["target_host"] != "api.github.com" {
		t.Errorf("expected target_host in params, got %v", params["target_host"])
	}
	if params["verdict_source"] != "deterministic" {
		t.Errorf("expected verdict_source=deterministic, got %v", params["verdict_source"])
	}
	if params["tool_name"] != "WebFetch" {
		t.Errorf("expected tool_name=WebFetch, got %v", params["tool_name"])
	}
	if params["tool_target"] != "https://api.github.com/repos/x/y/issues" {
		t.Errorf("expected tool_target URL, got %v", params["tool_target"])
	}
	toolInput, _ := params["tool_input"].(map[string]any)
	headers, _ := toolInput["headers"].(map[string]any)
	if _, ok := headers["Authorization"]; ok {
		t.Errorf("tool_input should not persist Authorization header: %+v", headers)
	}
}

// TestAuditEmitter_LogToolUseInspected_TaskID confirms that passing a
// non-empty taskID populates AuditEntry.TaskID — the field the
// dashboard's per-task activity feed filters on (GET /api/audit?task_id=).
// Without this linkage, lite-proxy tool_use rows never appear in any
// task's activity tab.
func TestAuditEmitter_LogToolUseInspected_TaskID(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.LogToolUseInspected(context.Background(), agent, "req-task", conversation.ToolUse{
		ID:    "toolu_2",
		Name:  "WebFetch",
		Input: json.RawMessage(`{"url":"https://api.github.com/repos/x/y"}`),
	}, inspector.Verdict{IsAPICall: true, Host: "api.github.com", Method: "GET", Path: "/repos/x/y", Source: inspector.SourceDeterministic},
		"allow", "matched_task", "scope covered", "task-abc")

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{TaskID: "task-abc"})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row filtered by task_id, got %d", len(rows))
	}
	if rows[0].TaskID == nil || *rows[0].TaskID != "task-abc" {
		t.Errorf("task_id not persisted: %v", rows[0].TaskID)
	}
}

func TestAuditEmitter_LogResolverSwap(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.LogResolverSwap(context.Background(), agent, "req-2",
		"autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "github", "api.github.com", "/repos/x/y", "POST",
		201, "allow", "success", "",
		7*time.Millisecond)

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.Service != "github" {
		t.Errorf("service=%q, want github", row.Service)
	}
	if row.Action != "lite_proxy.resolver.POST" {
		t.Errorf("action=%q", row.Action)
	}
	if row.DurationMS != 7 {
		t.Errorf("duration_ms=%d, want 7", row.DurationMS)
	}
	var params map[string]any
	_ = json.Unmarshal(row.ParamsSafe, &params)
	if params["target_host"] != "api.github.com" {
		t.Errorf("expected target_host=api.github.com, got %v", params["target_host"])
	}
	if params["placeholder"] != "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" {
		t.Errorf("expected placeholder in params, got %v", params["placeholder"])
	}
}

// PromptSHA stub used to wire forensic field plumbing.
type promptSHAStub struct{ sha string }

func (p promptSHAStub) PromptSHA() string { return p.sha }

func TestAuditEmitter_PopulatesValidatorPromptSHA(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, promptSHAStub{sha: "abc123"})

	em.LogEndpointCall(context.Background(), agent, "req-1", "anthropic", "lite_proxy.messages.create",
		200, "allow", "success", "", 0, nil, EndpointCallExtras{})

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	var params map[string]any
	_ = json.Unmarshal(rows[0].ParamsSafe, &params)
	if params["validator_prompt"] != "abc123" {
		t.Errorf("expected validator_prompt=abc123, got %v", params["validator_prompt"])
	}
}

// Security: credentials embedded in audit values (not just keys) must
// be redacted. A `command` value containing `Bearer ghp_...` previously
// landed verbatim in the audit row.
func TestRedactSecretsInString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bearer_token_in_command",
			in:   "curl -H 'Authorization: Bearer ghp_realtoken123' https://api.github.com/x",
			want: "curl -H 'Authorization: <REDACTED:auth>' https://api.github.com/x",
		},
		{
			name: "anthropic_key",
			in:   "use sk-ant-realsecret as the key",
			want: "use <REDACTED:auth> as the key",
		},
		{
			name: "openai_key",
			in:   "OPENAI_API_KEY=sk-proj-realsecret123",
			want: "OPENAI_API_KEY=<REDACTED:auth>",
		},
		{
			name: "agent_token",
			in:   "agent token is cvis_abc123def",
			want: "agent token is <REDACTED:auth>",
		},
		{
			name: "url_basic_auth",
			in:   "https://user:secretpw@api.example.com/path",
			want: "https://<REDACTED:auth>@api.example.com/path",
		},
		{
			name: "autovault_placeholder_survives",
			in:   "use placeholder autovault_github_xyz123 here",
			want: "use placeholder autovault_github_xyz123 here",
		},
		{
			name: "github_fine_grained_pat",
			in:   "TOKEN=github_pat_11ABCDEF0_realfinegrainedpatsecret",
			want: "TOKEN=<REDACTED:auth>",
		},
		{
			name: "github_refresh_token",
			in:   "refresh=ghr_realgithubrefreshtoken123",
			want: "refresh=<REDACTED:auth>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecretsInString(tc.in)
			if got != tc.want {
				t.Errorf("redaction:\n  in=  %q\n  got= %q\n  want=%q", tc.in, got, tc.want)
			}
		})
	}
}
