package llmproxy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
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
		200, "allow", "success", "", 12*time.Millisecond, map[string]any{"input_tokens": 18, "output_tokens": 8})

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
		Placeholders: []string{"autovault_github_xxx"},
		CredentialLocations: []inspector.CredentialLocation{
			{Kind: "header", Name: "Authorization", Scheme: "Bearer"},
		},
	}
	em.LogToolUseInspected(context.Background(), agent, "req-1", conversation.ToolUse{
		ID:    "toolu_1",
		Name:  "WebFetch",
		Input: json.RawMessage(`{"url":"https://api.github.com/repos/x/y/issues","headers":{"Authorization":"Bearer secret"}}`),
	}, verdict, "rewrite", "success", verdict.Reason)

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

func TestAuditEmitter_LogResolverSwap(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.LogResolverSwap(context.Background(), agent, "req-2",
		"autovault_github_xxx", "github", "api.github.com", "/repos/x/y", "POST",
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
	if params["placeholder"] != "autovault_github_xxx" {
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
		200, "allow", "success", "", 0, nil)

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
