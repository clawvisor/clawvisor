package llmproxy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/runtime/toolnames"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

// seedPostprocessStore returns a store with a github placeholder + agent
// owned by `userID/agentID`. Tests that rely on the boundary check pass
// the placeholder string into their tool_use input.
func seedPostprocessStore(t *testing.T, placeholder string) (store.Store, string, string) {
	return seedPostprocessStoreWithService(t, placeholder, "github")
}

func seedPostprocessStoreWithService(t *testing.T, placeholder, serviceID string) (store.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "post.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "post@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "agent-token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := st.CreateRuntimePlaceholder(ctx, &store.RuntimePlaceholder{
		Placeholder: placeholder,
		UserID:      user.ID,
		AgentID:     agent.ID,
		ServiceID:   serviceID,
		VaultItemID: serviceID,
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}
	return st, user.ID, agent.ID
}

func anthropicJSONWithToolUse(input string) []byte {
	return anthropicJSONWithNamedToolUse("WebFetch", input)
}

func anthropicJSONWithNamedToolUse(name, input string) []byte {
	return []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"text","text":"sure"},
			{"type":"tool_use","id":"toolu_1","name":"` + name + `","input":` + input + `}
		],
		"stop_reason":"tool_use"
	}`)
}

func TestPostprocess_JSONNoTrigger(t *testing.T) {
	body := anthropicJSONWithToolUse(`{"url":"https://example.com/foo"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
	})

	if got.Rewritten {
		t.Fatalf("no autovault placeholder should produce no rewrite")
	}
	if string(got.Body) != string(body) {
		t.Fatalf("body should be unchanged when nothing triggers")
	}
}

func TestPostprocess_AuditsNoTriggerToolUse(t *testing.T) {
	body := anthropicJSONWithToolUse(`{"url":"https://example.com/foo"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
		Audit:        NewAuditEmitter(st, nil, nil),
		RequestID:    "req-audit",
	})

	if got.Rewritten {
		t.Fatalf("no autovault placeholder should produce no rewrite")
	}
	rows, _, err := st.ListAuditEntries(req.Context(), userID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	row := rows[0]
	if row.Service != "runtime.tool_use" {
		t.Fatalf("service=%q, want runtime.tool_use", row.Service)
	}
	if row.Action != "lite_proxy.tool_use.allow" {
		t.Fatalf("action=%q, want lite_proxy.tool_use.allow", row.Action)
	}
	if row.ToolUseID == nil || *row.ToolUseID != "toolu_1" {
		t.Fatalf("tool_use_id=%v, want toolu_1", row.ToolUseID)
	}
	var params map[string]any
	if err := json.Unmarshal(row.ParamsSafe, &params); err != nil {
		t.Fatalf("params unmarshal: %v", err)
	}
	if params["tool_name"] != "WebFetch" {
		t.Fatalf("tool_name=%v, want WebFetch", params["tool_name"])
	}
	if params["tool_target"] != "https://example.com/foo" {
		t.Fatalf("tool_target=%v, want https://example.com/foo", params["tool_target"])
	}
	if params["verdict_source"] != "trigger_miss" {
		t.Fatalf("verdict_source=%v, want trigger_miss", params["verdict_source"])
	}
}

func TestPostprocess_SourceTriggerMissHonorsToolDenyRule(t *testing.T) {
	body := anthropicJSONWithToolUse(`{"url":"https://example.com/foo"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
		ToolRules: []*store.RuntimePolicyRule{{
			ID:       "deny-webfetch",
			UserID:   userID,
			AgentID:  &agentID,
			Kind:     "tool",
			Action:   "deny",
			ToolName: "WebFetch",
			Reason:   "web fetch blocked",
			Enabled:  true,
		}},
	})

	if !got.Rewritten {
		t.Fatalf("tool deny rule should rewrite the tool_use to a refusal")
	}
	if !strings.Contains(string(got.Body), "web fetch blocked") {
		t.Fatalf("refusal missing rule reason: %s", got.Body)
	}
}

func TestBoundaryCheckVerdictUnknownServiceFailsClosed(t *testing.T) {
	placeholder := "autovault_agentphone_xxx"
	st, userID, agentID := seedPostprocessStoreWithService(t, placeholder, "agentphone")
	req := httptest.NewRequest("POST", "/v1/messages", nil)

	reason, ok := boundaryCheckVerdict(req, PostprocessConfig{
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
	}, inspector.Verdict{
		IsAPICall:    true,
		Host:         "api.agentphone.ai",
		Placeholders: []string{placeholder},
	})

	if ok {
		t.Fatalf("expected unknown service to fail closed, got reason %q", reason)
	}
	if !strings.Contains(reason, "no bound-service hosts") {
		t.Fatalf("expected missing bound-host reason, got %q", reason)
	}
}

func TestPostprocess_ReadOnlyBashBypassesTaskScopeByDefault(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"pwd", "pwd"},
		{"ls", "ls -la /tmp"},
		{"cat", "cat /etc/hosts"},
		{"pipe_grep_wc", "ls /tmp | grep landing | wc -l"},
		{"find", "find . -name '*.go'"},
		{"head", "head -n 20 README.md"},
		{"stderr_to_null", "ls /missing 2>/dev/null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := anthropicJSONWithNamedToolUse("Bash", `{"command":`+jsonString(tc.cmd)+`}`)
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
			st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

			got := Postprocess(req, body, "application/json", PostprocessConfig{
				Inspector:        insp,
				RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
				CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
				Store:            st,
				AgentUserID:      userID,
				AgentID:          agentID,
				Audit:            NewAuditEmitter(st, nil, nil),
				RequestID:        "req-bash-readonly-" + tc.name,
				CandidateTasks:   []*store.Task{}, // no task scope
				ToolRules:        []*store.RuntimePolicyRule{},
				EgressRules:      []*store.RuntimePolicyRule{},
				PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
				Posture:          runtimedecision.PostureEnforce,
			})

			if got.Rewritten {
				t.Fatalf("read-only bash %q should pass through; got rewrite body=%s", tc.cmd, got.Body)
			}
			rows, _, err := st.ListAuditEntries(req.Context(), userID, store.AuditFilter{})
			if err != nil {
				t.Fatalf("ListAuditEntries: %v", err)
			}
			if len(rows) != 1 || rows[0].Outcome != "readonly_shell_pass_through" {
				t.Fatalf("expected readonly_shell_pass_through, got %d rows outcome=%q", len(rows), rows[0].Outcome)
			}
		})
	}
}

func TestPostprocess_ReadOnlyBashCanBeDisabledByPolicy(t *testing.T) {
	body := anthropicJSONWithNamedToolUse("Bash", `{"command":"ls -la /tmp"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")
	agentRuleID := "readonly-shell-disabled"

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:      insp,
		RewriteOpts:    inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:   NewMemoryCallerNonceCache(time.Minute),
		Store:          st,
		AgentUserID:    userID,
		AgentID:        agentID,
		Audit:          NewAuditEmitter(st, nil, nil),
		RequestID:      "req-bash-readonly-disabled",
		CandidateTasks: []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{{
			ID:         agentRuleID,
			UserID:     userID,
			AgentID:    &agentID,
			Kind:       "tool",
			Action:     "deny",
			ToolName:   "Bash",
			InputShape: toolnames.ReadOnlyShellSettingInputShape(),
			Source:     toolnames.ReadOnlyShellSettingSource,
			Enabled:    true,
		}},
		EgressRules:      []*store.RuntimePolicyRule{},
		PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
		Posture:          runtimedecision.PostureEnforce,
	})

	if !got.Rewritten {
		t.Fatalf("read-only bash should require approval when disabled")
	}
}

// Codex's `write_stdin` with empty `chars` is the harness polling a
// backgrounded shell for output — equivalent to BashOutput. It must
// pass through without a task scope. Non-empty chars (actual typed
// input) still gates.
func TestPostprocess_WriteStdinPollBypassesTaskScope(t *testing.T) {
	body := anthropicJSONWithNamedToolUse("write_stdin",
		`{"session_id":79860,"chars":"","max_output_tokens":1200,"yield_time_ms":1000}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		Audit:            NewAuditEmitter(st, nil, nil),
		RequestID:        "req-write-stdin-poll",
		CandidateTasks:   []*store.Task{},
		ToolRules:        []*store.RuntimePolicyRule{},
		EgressRules:      []*store.RuntimePolicyRule{},
		PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
		Posture:          runtimedecision.PostureEnforce,
	})
	if got.Rewritten {
		t.Fatalf("write_stdin poll must pass through; got rewrite body=%s", got.Body)
	}
	rows, _, err := st.ListAuditEntries(req.Context(), userID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) != 1 || rows[0].Outcome != "shell_poll_pass_through" {
		t.Errorf("expected outcome=shell_poll_pass_through, got %d rows; outcome=%q", len(rows), rows[0].Outcome)
	}
}

// Negative: write_stdin with non-empty chars is the model typing into
// a shell — could be running `rm`. Must still gate.
func TestPostprocess_WriteStdinWithCharsStillRequiresApproval(t *testing.T) {
	body := anthropicJSONWithNamedToolUse("write_stdin",
		`{"session_id":79860,"chars":"rm -rf /tmp/x\n","max_output_tokens":1200,"yield_time_ms":1000}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		Audit:            NewAuditEmitter(st, nil, nil),
		RequestID:        "req-write-stdin-active",
		CandidateTasks:   []*store.Task{},
		ToolRules:        []*store.RuntimePolicyRule{},
		EgressRules:      []*store.RuntimePolicyRule{},
		PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
		Posture:          runtimedecision.PostureEnforce,
	})
	if !got.Rewritten {
		t.Fatalf("write_stdin with non-empty chars must require approval, got pass-through")
	}
}

// Negative: write-mutating / network bash commands must still gate
// on task scope. The classifier is the only thing standing between
// "no task" and "permitted to run anything in shell."
func TestPostprocess_MutatingBashStillRequiresApproval(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"rm", "rm -rf /tmp/x"},
		{"mkdir", "mkdir /tmp/new"},
		{"curl", "curl https://example.com"},
		{"sed_inplace", "sed -i s/x/y/ file"},
		{"write_redirect", "ls > /tmp/out"},
		{"chain_to_mutation", "pwd && rm /tmp/x"},
		{"cmd_subst", "echo $(rm /tmp/x)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := anthropicJSONWithNamedToolUse("Bash", `{"command":`+jsonString(tc.cmd)+`}`)
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
			st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

			got := Postprocess(req, body, "application/json", PostprocessConfig{
				Inspector:        insp,
				RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
				CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
				Store:            st,
				AgentUserID:      userID,
				AgentID:          agentID,
				Audit:            NewAuditEmitter(st, nil, nil),
				RequestID:        "req-bash-mutating-" + tc.name,
				CandidateTasks:   []*store.Task{},
				ToolRules:        []*store.RuntimePolicyRule{},
				EgressRules:      []*store.RuntimePolicyRule{},
				PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
				Posture:          runtimedecision.PostureEnforce,
			})
			if !got.Rewritten {
				t.Fatalf("mutating/network bash %q must require approval, got pass-through", tc.cmd)
			}
		})
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestPostprocess_ReadOnlyToolPolicyAllowlistBypassesTaskScope(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args string
	}{
		{"read_file", "Read", `{"file_path":"/tmp/foo.txt"}`},
		{"glob", "Glob", `{"pattern":"**/*.go"}`},
		{"codex_read_file", "read_file", `{"path":"/tmp/foo.txt"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := anthropicJSONWithNamedToolUse(tc.tool, tc.args)
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
			st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

			got := Postprocess(req, body, "application/json", PostprocessConfig{
				Inspector:      insp,
				RewriteOpts:    inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
				CallerNonces:   NewMemoryCallerNonceCache(time.Minute),
				Store:          st,
				AgentUserID:    userID,
				AgentID:        agentID,
				Audit:          NewAuditEmitter(st, nil, nil),
				RequestID:      "req-local-" + tc.name,
				CandidateTasks: []*store.Task{}, // no task scope set
				ToolRules: []*store.RuntimePolicyRule{{
					ID:         "allow-" + tc.name,
					UserID:     userID,
					AgentID:    &agentID,
					Kind:       "tool",
					Action:     "allow",
					ToolName:   tc.tool,
					InputShape: json.RawMessage(`{}`),
					Enabled:    true,
				}},
				EgressRules:      []*store.RuntimePolicyRule{},
				PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
				Posture:          runtimedecision.PostureEnforce,
			})

			if got.Rewritten {
				t.Fatalf("allowlisted read-only tool %q should pass through, got rewrite body=%s", tc.tool, got.Body)
			}
			rows, _, err := st.ListAuditEntries(req.Context(), userID, store.AuditFilter{})
			if err != nil {
				t.Fatalf("ListAuditEntries: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("expected 1 audit row, got %d", len(rows))
			}
			if rows[0].Outcome != "rule_allow" {
				t.Errorf("expected policy allow outcome, got %q", rows[0].Outcome)
			}
		})
	}
}

// Negative: an HTTP-shaped tool (Bash) without a covering task must
// still hit the approval prompt — the local-only bypass must not
// leak to tools that can actually transmit credentials.
func TestPostprocess_BashWithoutTaskScopeStillRequiresApproval(t *testing.T) {
	// Mutating bash (mkdir) must still require approval — only the
	// AST-classified read-only commands bypass scope. Bare `echo` is
	// now read-only and would pass through; we want this test to
	// guard the scope-required-for-mutations contract.
	body := anthropicJSONWithNamedToolUse("Bash", `{"command":"mkdir /tmp/something"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		Audit:            NewAuditEmitter(st, nil, nil),
		RequestID:        "req-bash-still-gated",
		CandidateTasks:   []*store.Task{},
		ToolRules:        []*store.RuntimePolicyRule{},
		EgressRules:      []*store.RuntimePolicyRule{},
		PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
		Posture:          runtimedecision.PostureEnforce,
	})

	if !got.Rewritten {
		t.Fatalf("Bash without task scope must require approval, got pass-through")
	}
}

func TestPostprocess_SourceTriggerMissRequiresApprovalWhenScopeMissing(t *testing.T) {
	// Use a mutating command (mkdir) so it doesn't get caught by the
	// read-only bash bypass. The test asserts that scope-missing
	// produces an approval prompt for tools that need it.
	body := anthropicJSONWithNamedToolUse("Bash", `{"command":"mkdir -p /tmp/greet","description":"Create greet workspace"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		Audit:            NewAuditEmitter(st, nil, nil),
		RequestID:        "req-missing-scope",
		CandidateTasks:   []*store.Task{},
		ToolRules:        []*store.RuntimePolicyRule{},
		EgressRules:      []*store.RuntimePolicyRule{},
		PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
		Posture:          runtimedecision.PostureEnforce,
	})

	if !got.Rewritten {
		t.Fatalf("missing task/rule scope should rewrite to an approval prompt")
	}
	text := anthropicResponseText(t, got.Body)
	if !strings.Contains(text, "Reply `yes` or `y`") ||
		!strings.Contains(text, "`task`") ||
		!strings.Contains(text, "no matching task scope") {
		t.Fatalf("approval prompt missing expected text: %s", got.Body)
	}
	if strings.Contains(text, "https://clawvisor.local/control/tasks") {
		t.Fatalf("approval prompt should defer task details until user replies task: %s", got.Body)
	}
	rows, _, err := st.ListAuditEntries(req.Context(), userID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "lite_proxy.tool_use.block" || row.Outcome != "task_scope_missing" {
		t.Fatalf("unexpected audit row: action=%s outcome=%s", row.Action, row.Outcome)
	}
}

func TestPostprocess_ToolTaskIntentRefusalRequiresApproval(t *testing.T) {
	body := anthropicJSONWithNamedToolUse("Write", `{"file_path":"/tmp/goodbye.py","content":"print('bye')\n"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")
	task := &store.Task{
		ID:                     "task-1",
		AgentID:                agentID,
		Purpose:                "Create and refactor /tmp/hello.py",
		Status:                 "active",
		IntentVerificationMode: "strict",
		ExpectedTools:          json.RawMessage(`[{"tool_name":"Write","why":"create and refactor /tmp/hello.py"}]`),
	}
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: false, Explanation: "The requested file path /tmp/goodbye.py and content do not match the task purpose of creating and refactoring /tmp/hello.py."}}

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		CandidateTasks:   []*store.Task{task},
		ToolRules:        []*store.RuntimePolicyRule{},
		EgressRules:      []*store.RuntimePolicyRule{},
		IntentVerifier:   verifier,
		PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
		Posture:          runtimedecision.PostureEnforce,
	})

	if !got.Rewritten {
		t.Fatalf("intent refusal should rewrite to an approval prompt")
	}
	text := anthropicResponseText(t, got.Body)
	if !strings.Contains(text, "Reply `yes` or `y`") ||
		!strings.Contains(text, "/tmp/goodbye.py") ||
		!strings.Contains(text, "do not match the task purpose") {
		t.Fatalf("intent refusal prompt missing expected text: %s", got.Body)
	}
	if strings.Contains(text, "Tool use was blocked") {
		t.Fatalf("intent refusal should not hard block: %s", got.Body)
	}
}

func anthropicResponseText(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("response unmarshal: %v", err)
	}
	var parts []string
	for _, block := range resp.Content {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestApprovalPromptMentionsTaskReply(t *testing.T) {
	got := approvalPrompt(conversation.ToolUse{
		Name:  "Write",
		Input: json.RawMessage(`{"file_path":"/tmp/report.txt","content":"hello"}`),
	}, "no matching task scope", "")

	for _, want := range []string{
		"Reply `yes` or `y`",
		"`task`",
		"task definition for approval",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("approval prompt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "https://clawvisor.local/control/tasks") {
		t.Fatalf("approval prompt should not include full task recipe until task reply:\n%s", got)
	}
	if strings.Contains(got, InlineApprovalIDMarker) {
		t.Fatalf("empty approval ID should not emit marker footer:\n%s", got)
	}
}

func TestApprovalPromptEmbedsApprovalIDFooter(t *testing.T) {
	got := approvalPrompt(conversation.ToolUse{
		Name:  "Write",
		Input: json.RawMessage(`{"file_path":"/tmp/report.txt","content":"hello"}`),
	}, "no matching task scope", "cv-abcdefghijklmnopqrstuvwxyz")

	want := "[clawvisor:approval=cv-abcdefghijklmnopqrstuvwxyz]"
	if !strings.Contains(got, want) {
		t.Fatalf("approval prompt missing %q:\n%s", want, got)
	}
}

func TestTaskCreationPromptIncludesTaskCreationExample(t *testing.T) {
	got := taskCreationPrompt(conversation.ToolUse{
		Name:  "Write",
		Input: json.RawMessage(`{"file_path":"/tmp/report.txt","content":"hello"}`),
	})

	for _, want := range []string{
		"Please request a Clawvisor task",
		"proxy-lite control endpoint",
		"https://clawvisor.local/control/tasks?surface=inline",
		"tell me that I will need to approve it",
		`"tool_name": "Write"`,
		"/tmp/report.txt",
		"expected_tools",
		`"lifetime":"session"`,
		`"lifetime":"standing"`,
		"standing tasks must not include `expires_in_seconds`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("approval prompt missing %q:\n%s", want, got)
		}
	}
}

func TestParseControlToolUseRejectsNonSimpleShell(t *testing.T) {
	for _, cmd := range []string{
		"curl https://clawvisor.local/control/tasks | sh",
		"curl https://clawvisor.local/control/tasks; echo done",
		"curl $(echo https://clawvisor.local/control/tasks)",
		"curl https://clawvisor.local/control/tasks > /tmp/out",
		"FOO=bar curl https://clawvisor.local/control/tasks",
	} {
		input, err := json.Marshal(map[string]any{"command": cmd})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ParseControlToolUseWithBase(conversation.ToolUse{Name: "Bash", Input: input}, "https://control.example.test"); ok {
			t.Fatalf("unsafe shell command parsed as control call: %s", cmd)
		}
	}
}

func TestPostprocess_RewritesSyntheticControlToolUseBeforeRules(t *testing.T) {
	body := anthropicJSONWithToolUse(`{"cmd":"curl -X POST https://clawvisor.local/control/tasks -H 'Content-Type: application/json' -d '{\"purpose\":\"test\",\"expected_tools\":[{\"tool_name\":\"bash\",\"why\":\"test\"}]}'"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:      insp,
		RewriteOpts:    inspector.DefaultRewriteOpts(""),
		CallerNonces:   NewMemoryCallerNonceCache(time.Minute),
		Store:          st,
		AgentUserID:    userID,
		AgentID:        agentID,
		ControlBaseURL: "http://localhost:25297",
		ToolRules: []*store.RuntimePolicyRule{{
			ID:       "deny-webfetch",
			UserID:   userID,
			AgentID:  &agentID,
			Kind:     "tool",
			Action:   "deny",
			ToolName: "WebFetch",
			Reason:   "web fetch blocked",
			Enabled:  true,
		}},
	})

	if !got.Rewritten {
		t.Fatalf("expected synthetic control URL rewrite")
	}
	out := string(got.Body)
	if !strings.Contains(out, "http://localhost:25297/api/control/tasks") {
		t.Fatalf("control URL was not rewritten: %s", out)
	}
	if !strings.Contains(out, "X-Clawvisor-Target-Host") {
		t.Fatalf("control rewrite missing target header: %s", out)
	}
	if strings.Contains(out, "web fetch blocked") {
		t.Fatalf("synthetic control endpoint should bypass tool rules: %s", out)
	}
}

func TestPostprocess_RewritesConfiguredControlURLBeforeRules(t *testing.T) {
	body := anthropicJSONWithToolUse(`{"cmd":"curl -i -X POST -H 'Content-Type: application/json' -H 'X-Clawvisor-Target-Host: clawvisor.local' -H 'X-Clawvisor-Caller: Bearer cvis_test' --data '{\"purpose\":\"Create a sample permission task from the shell for control-plane verification.\",\"intent_verification_mode\":\"strict\",\"expires_in_seconds\":600,\"expected_tools\":[{\"tool_name\":\"bash\",\"why\":\"Run curl against the proxied Clawvisor control endpoints.\"}]}' https://control.example.test/api/control/tasks"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")
	opts := inspector.DefaultRewriteOpts("https://control.example.test")
	opts.CallerToken = "cvis_test"

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:      insp,
		RewriteOpts:    opts,
		CallerNonces:   NewMemoryCallerNonceCache(time.Minute),
		Store:          st,
		AgentUserID:    userID,
		AgentID:        agentID,
		ControlBaseURL: "https://control.example.test",
		ToolRules: []*store.RuntimePolicyRule{{
			ID:       "deny-webfetch",
			UserID:   userID,
			AgentID:  &agentID,
			Kind:     "tool",
			Action:   "deny",
			ToolName: "WebFetch",
			Reason:   "web fetch blocked",
			Enabled:  true,
		}},
	})

	if !got.Rewritten {
		t.Fatalf("expected configured control URL rewrite")
	}
	out := string(got.Body)
	if !strings.Contains(out, "https://control.example.test/api/control/tasks") {
		t.Fatalf("control URL missing after rewrite: %s", out)
	}
	if !strings.Contains(out, "X-Clawvisor-Caller") {
		t.Fatalf("control rewrite missing caller header: %s", out)
	}
	if strings.Contains(out, "web fetch blocked") {
		t.Fatalf("configured control endpoint should bypass tool rules: %s", out)
	}
}

func TestPostprocess_RewritesMultilineConfiguredControlURLBeforeRules(t *testing.T) {
	body := anthropicJSONWithToolUse("{\"cmd\":\"curl -i -X POST \\\\\\n-H 'Content-Type: application/json' \\\\\\n--data '{\\\"purpose\\\":\\\"test\\\",\\\"expected_tools\\\":[{\\\"tool_name\\\":\\\"bash\\\",\\\"why\\\":\\\"test\\\"}]}' \\\\\\nhttps://control.example.test/api/control/tasks\"}")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")
	opts := inspector.DefaultRewriteOpts("https://control.example.test")
	opts.CallerToken = "cvis_test"

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:      insp,
		RewriteOpts:    opts,
		CallerNonces:   NewMemoryCallerNonceCache(time.Minute),
		Store:          st,
		AgentUserID:    userID,
		AgentID:        agentID,
		ControlBaseURL: "https://control.example.test",
		ToolRules: []*store.RuntimePolicyRule{{
			ID:       "deny-webfetch",
			UserID:   userID,
			AgentID:  &agentID,
			Kind:     "tool",
			Action:   "deny",
			ToolName: "WebFetch",
			Reason:   "web fetch blocked",
			Enabled:  true,
		}},
	})

	if !got.Rewritten {
		t.Fatalf("expected multiline configured control URL rewrite")
	}
	if strings.Contains(string(got.Body), "web fetch blocked") {
		t.Fatalf("multiline configured control endpoint should bypass tool rules: %s", got.Body)
	}
}

func TestPostprocess_RewritesHeredocSyntheticControlURLBeforeRules(t *testing.T) {
	cmd := "curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120' \\\n  -H 'Content-Type: application/json' \\\n  --data @- <<'JSON'\n{\"purpose\":\"test\",\"expected_tools\":[{\"tool_name\":\"Bash\",\"why\":\"Search with find /tmp -iname 'hello'; content can mention $HOME\"}]}\nJSON"
	input, err := json.Marshal(map[string]any{"command": cmd})
	if err != nil {
		t.Fatal(err)
	}
	body := anthropicJSONWithNamedToolUse("Bash", string(input))
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")
	opts := inspector.DefaultRewriteOpts("https://control.example.test")
	opts.CallerToken = "cvis_test"

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:      insp,
		RewriteOpts:    opts,
		CallerNonces:   NewMemoryCallerNonceCache(time.Minute),
		Store:          st,
		AgentUserID:    userID,
		AgentID:        agentID,
		ControlBaseURL: "https://control.example.test",
		ToolRules: []*store.RuntimePolicyRule{{
			ID:       "deny-bash",
			UserID:   userID,
			AgentID:  &agentID,
			Kind:     "tool",
			Action:   "deny",
			ToolName: "Bash",
			Reason:   "bash blocked",
			Enabled:  true,
		}},
	})

	if !got.Rewritten {
		t.Fatalf("expected heredoc synthetic control URL rewrite")
	}
	out := string(got.Body)
	if strings.Contains(out, "bash blocked") {
		t.Fatalf("heredoc synthetic control endpoint should bypass tool rules: %s", out)
	}
	if !strings.Contains(out, "'https://control.example.test/api/control/tasks?wait=true\\u0026timeout=120'") ||
		!strings.Contains(out, "X-Clawvisor-Target-Host") ||
		!strings.Contains(out, "X-Clawvisor-Caller") ||
		!strings.Contains(out, `\u003c\u003c'JSON'`) {
		t.Fatalf("heredoc control rewrite lost expected command shape: %s", out)
	}
}

func TestPostprocess_MalformedSyntheticControlCommandRewritesToToolFailure(t *testing.T) {
	cmd := `curl -sS -H 'X-Clawvisor-Caller: Bearer cv-nonce-stale123' 'https://clawvisor.local/control/services' | python3 -c 'print("filter")'`
	input, err := json.Marshal(map[string]any{
		"command":     cmd,
		"description": "Find agentphone service definition",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := anthropicJSONWithNamedToolUse("Bash", string(input))
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:      insp,
		RewriteOpts:    inspector.DefaultRewriteOpts(""),
		CallerNonces:   NewMemoryCallerNonceCache(time.Minute),
		Store:          st,
		AgentUserID:    userID,
		AgentID:        agentID,
		ControlBaseURL: "http://localhost:25297",
	})

	if !got.Rewritten {
		t.Fatalf("expected malformed control command failure rewrite")
	}
	out := string(got.Body)
	if !strings.Contains(out, "/api/control/failure") {
		t.Fatalf("expected control failure endpoint rewrite, got: %s", out)
	}
	if !strings.Contains(out, "original_command") || !strings.Contains(out, "python3") {
		t.Fatalf("expected rewritten failure call to include original command context, got: %s", out)
	}
	if strings.Contains(out, "cv-nonce-stale123") || !strings.Contains(out, "Bearer REDACTED") {
		t.Fatalf("expected stale nonce in original command to be redacted, got: %s", out)
	}
	if strings.Contains(out, "no matching task scope") {
		t.Fatalf("malformed control command should not fall through to task-scope refusal: %s", out)
	}
	if strings.Contains(out, "control endpoint rewrite refused") {
		t.Fatalf("malformed control command should return tool output instead of an assistant refusal: %s", out)
	}
}

func TestPostprocess_CoalescesMultipleApprovalsIntoSingleHold(t *testing.T) {
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{"url":"https://example.com/one"}},
			{"type":"tool_use","id":"toolu_2","name":"WebFetch","input":{"url":"https://example.com/two"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")
	cache := NewMemoryPendingApprovalCache(time.Minute)

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules:        []*store.RuntimePolicyRule{{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true}},
		EgressRules:      []*store.RuntimePolicyRule{},
	})
	if !got.Rewritten {
		t.Fatalf("expected coalesced approval prompt for reviewed tool calls")
	}
	out := string(got.Body)
	if !strings.Contains(out, "Clawvisor paused this turn for approval (2 tool calls).") {
		t.Fatalf("expected coalesced prompt header, got: %s", out)
	}
	if !strings.Contains(out, "https://example.com/one") || !strings.Contains(out, "https://example.com/two") {
		t.Fatalf("coalesced prompt should describe every held call, got: %s", out)
	}
	if !strings.Contains(out, "`task` to scope this work under a Clawvisor task") {
		t.Fatalf("coalesced prompt must offer the `task` verb so the user can promote a batch into a durable scope, got: %s", out)
	}

	// Only ONE hold is created for the whole turn; both held tool_uses
	// live under it (primary + Additional). A single user yes/no
	// releases or denies all of them together.
	holds := cache.snapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic)
	if len(holds) != 1 {
		t.Fatalf("expected exactly one coalesced hold, got %d: %+v", len(holds), holds)
	}
	hold := holds[0]
	if !hold.IsCoalesced() {
		t.Fatalf("expected hold.IsCoalesced() true; got primary=%s additional=%d", hold.ToolUse.ID, len(hold.Additional))
	}
	all := hold.AllHolds()
	if len(all) != 2 {
		t.Fatalf("expected 2 held tool_uses in the coalesced hold, got %d", len(all))
	}
	if all[0].ToolUse.ID != "toolu_1" || all[1].ToolUse.ID != "toolu_2" {
		t.Fatalf("held tool_uses out of order: %s, %s (want toolu_1, toolu_2)", all[0].ToolUse.ID, all[1].ToolUse.ID)
	}
	for _, h := range all {
		if h.Kind != HeldKindApproval {
			t.Fatalf("expected all coalesced uses to be HeldKindApproval, got %q for %s", h.Kind, h.ToolUse.ID)
		}
	}
}

func TestPostprocess_ObservePostureDoesNotBlockToolDenyRule(t *testing.T) {
	input := `{"url":"https://api.github.com/repos/x/y/issues","method":"POST","headers":{"Authorization":"Bearer autovault_github_xxx"}}`
	body := anthropicJSONWithToolUse(input)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
		Posture:      runtimedecision.PostureObserve,
		ToolRules: []*store.RuntimePolicyRule{{
			ID:       "deny-webfetch",
			UserID:   userID,
			AgentID:  &agentID,
			Kind:     "tool",
			Action:   "deny",
			ToolName: "WebFetch",
			Reason:   "web fetch blocked",
			Enabled:  true,
		}},
	})

	if !got.Rewritten {
		t.Fatalf("observe mode should still rewrite credentialed calls")
	}
	if strings.Contains(string(got.Body), "web fetch blocked") {
		t.Fatalf("observe mode should not block with rule reason: %s", got.Body)
	}
	if !strings.Contains(string(got.Body), "https://proxy.example/api/proxy/repos/x/y/issues") {
		t.Fatalf("observe mode should allow rewrite through proxy: %s", got.Body)
	}
}

func TestPostprocess_JSONRewritesAutovaultURL(t *testing.T) {
	input := `{"url":"https://api.github.com/repos/x/y/issues","method":"POST","headers":{"Authorization":"Bearer autovault_github_xxx"}}`
	body := anthropicJSONWithToolUse(input)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
	})

	if !got.Rewritten {
		t.Fatalf("expected rewrite when autovault placeholder present")
	}
	var resp struct {
		Content []struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(got.Body, &resp); err != nil {
		t.Fatalf("rewritten body not parseable JSON: %v", err)
	}
	for _, c := range resp.Content {
		if c.Type != "tool_use" {
			continue
		}
		var inputObj struct {
			URL     string         `json:"url"`
			Headers map[string]any `json:"headers"`
		}
		if err := json.Unmarshal(c.Input, &inputObj); err != nil {
			t.Fatalf("rewritten input not parseable: %v", err)
		}
		if !strings.HasPrefix(inputObj.URL, "https://proxy.example/api/proxy/repos/x/y/issues") {
			t.Fatalf("URL not rewritten to resolver: %q", inputObj.URL)
		}
		if inputObj.Headers["X-Clawvisor-Target-Host"] != "api.github.com" {
			t.Fatalf("expected X-Clawvisor-Target-Host header, got %+v", inputObj.Headers)
		}
		if inputObj.Headers["Authorization"] != "Bearer autovault_github_xxx" {
			t.Fatalf("placeholder should be preserved in headers, got %+v", inputObj.Headers)
		}
	}
}

func TestPostprocess_SSERewritesAutovaultURL(t *testing.T) {
	// A streamed Anthropic response with a tool_use block whose input
	// JSON contains an autovault_… placeholder. Rewriter should emit a
	// re-synthesized SSE stream with the URL pointing at the resolver.
	body := []byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"sure"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxx\"}}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "text/event-stream", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
	})

	if !got.Rewritten {
		t.Fatalf("expected SSE rewrite to fire on autovault placeholder")
	}
	out := string(got.Body)
	if !strings.Contains(out, "https://proxy.example/api/proxy/repos/x/y/issues") {
		t.Fatalf("rewritten SSE missing resolver URL:\n%s", out)
	}
	if !strings.Contains(out, "X-Clawvisor-Target-Host") {
		t.Fatalf("rewritten SSE missing X-Clawvisor-Target-Host header:\n%s", out)
	}
	if !strings.Contains(out, "Bearer autovault_github_xxx") {
		t.Fatalf("placeholder lost in SSE rewrite:\n%s", out)
	}
	if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("rewritten SSE missing required envelope events:\n%s", out)
	}
}

// OpenAI Responses API JSON rewrite — Codex's flagship transport.
func TestPostprocess_OpenAIResponsesJSONRewrite(t *testing.T) {
	body := []byte(`{
		"id":"resp_1",
		"object":"response",
		"model":"gpt-5",
		"output":[
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"WebFetch",
			 "arguments":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxx\"}}"}
		]
	}`)
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
	})

	if !got.Rewritten {
		t.Fatalf("expected rewrite for OpenAI Responses JSON, got skipped=%q", got.SkippedReason)
	}
	if !strings.Contains(string(got.Body), "https://proxy.example/api/proxy/repos/x/y/issues") {
		t.Fatalf("rewritten URL missing:\n%s", got.Body)
	}
	if !strings.Contains(string(got.Body), "X-Clawvisor-Target-Host") {
		t.Fatalf("X-Clawvisor-Target-Host missing:\n%s", got.Body)
	}
}

// OpenAI Responses API SSE rewrite — Codex defaults to streaming.
func TestPostprocess_OpenAIResponsesSSERewrite(t *testing.T) {
	body := []byte(`event: response.created
data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_1","name":"WebFetch"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxx\"}}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"name":"WebFetch","arguments":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxx\"}}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"WebFetch","arguments":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxx\"}}"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}

`)
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "text/event-stream", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
	})

	if !got.Rewritten {
		t.Fatalf("expected SSE rewrite for OpenAI Responses, got skipped=%q", got.SkippedReason)
	}
	out := string(got.Body)
	if !strings.Contains(out, "https://proxy.example/api/proxy/repos/x/y/issues") {
		t.Fatalf("rewritten URL missing:\n%s", out)
	}
	if !strings.Contains(out, "response.output_item.done") || !strings.Contains(out, "response.completed") {
		t.Fatalf("Responses SSE envelope missing:\n%s", out)
	}
	if !strings.Contains(out, "function_call_arguments.done") {
		t.Fatalf("function_call_arguments.done missing — Codex needs this signal:\n%s", out)
	}
}

func TestPostprocess_OpenAIResponsesSSEAuditsCustomToolCall(t *testing.T) {
	body := []byte(`event: response.created
data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","status":"in_progress","call_id":"call_patch","name":"apply_patch"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","status":"completed","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** Add File: /tmp/hello.sh\n+#!/bin/sh\n*** End Patch\n"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}

`)
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "text/event-stream", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
		Audit:        NewAuditEmitter(st, nil, nil),
		RequestID:    "req-custom-tool",
	})

	if got.Rewritten {
		t.Fatalf("custom tool call without credential trigger should not rewrite")
	}
	rows, _, err := st.ListAuditEntries(req.Context(), userID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "lite_proxy.tool_use.allow" {
		t.Fatalf("action=%q, want lite_proxy.tool_use.allow", row.Action)
	}
	if row.ToolUseID == nil || *row.ToolUseID != "call_patch" {
		t.Fatalf("tool_use_id=%v, want call_patch", row.ToolUseID)
	}
	var params map[string]any
	if err := json.Unmarshal(row.ParamsSafe, &params); err != nil {
		t.Fatalf("params unmarshal: %v", err)
	}
	if params["tool_name"] != "apply_patch" {
		t.Fatalf("tool_name=%v, want apply_patch", params["tool_name"])
	}
	input, ok := params["tool_input"].(map[string]any)
	if !ok || !strings.Contains(input["input"].(string), "/tmp/hello.sh") {
		t.Fatalf("tool_input=%v, want patch preview", params["tool_input"])
	}
}

// OpenAI Chat Completions JSON rewrite.
func TestPostprocess_OpenAIChatJSONRewrite(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl_1",
		"object":"chat.completion",
		"model":"gpt-5",
		"choices":[{
			"index":0,
			"message":{
				"role":"assistant",
				"tool_calls":[{
					"id":"call_1",
					"type":"function",
					"function":{
						"name":"WebFetch",
						"arguments":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxx\"}}"
					}
				}]
			},
			"finish_reason":"tool_calls"
		}]
	}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
	})

	if !got.Rewritten {
		t.Fatalf("expected rewrite for OpenAI Chat JSON, got skipped=%q", got.SkippedReason)
	}
	if !strings.Contains(string(got.Body), "https://proxy.example/api/proxy/repos/x/y/issues") {
		t.Fatalf("rewritten URL missing:\n%s", got.Body)
	}
}

// OpenAI Chat Completions SSE rewrite.
func TestPostprocess_OpenAIChatSSERewrite(t *testing.T) {
	body := []byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"WebFetch","arguments":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxx\"}}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "text/event-stream", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
	})

	if !got.Rewritten {
		t.Fatalf("expected SSE rewrite for OpenAI Chat, got skipped=%q", got.SkippedReason)
	}
	out := string(got.Body)
	if !strings.Contains(out, "https://proxy.example/api/proxy/repos/x/y/issues") {
		t.Fatalf("rewritten URL missing:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason=tool_calls missing:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("[DONE] terminator missing:\n%s", out)
	}
}

func TestPostprocess_AmbiguousFailsClosed(t *testing.T) {
	// A tool_use with autovault placeholder in a shape the deterministic
	// parser can't classify. The AmbiguousValidator returns ambiguous,
	// so the response should be replaced with a blocked-explanation text.
	input := `{"unknown_field":"autovault_github_xxx"}`
	body := anthropicJSONWithToolUse(input)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
	})

	// "Block" path of the existing rewriter replaces the content with text.
	if !got.Rewritten {
		t.Fatalf("expected rewrite-to-blocked when ambiguous")
	}
	if !strings.Contains(string(got.Body), "Clawvisor") {
		t.Fatalf("expected blocked-explanation text, got %q", string(got.Body))
	}
}
