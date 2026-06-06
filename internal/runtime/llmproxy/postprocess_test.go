package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

type postprocessFakeValidator struct {
	verdict inspector.Verdict
}

func (v postprocessFakeValidator) Validate(context.Context, inspector.ToolUse) (inspector.Verdict, error) {
	return v.verdict, nil
}

func TestPostprocessStream_BlockedAnthropicPromptUsesNextContentIndex(t *testing.T) {
	placeholder := "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	st, userID, agentID := seedPostprocessStore(t, placeholder)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	input := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[]}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"First"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"Second"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer ` + placeholder + `\"}}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":2}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var output bytes.Buffer
	result, err := PostprocessStream(context.Background(), req, strings.NewReader(input), &output, "text/event-stream", PostprocessConfig{
		Inspector:   insp,
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
	})
	if err != nil {
		t.Fatalf("PostprocessStream: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected blocked tool to rewrite stream with approval text")
	}
	out := output.String()
	if !strings.Contains(out, "Tool use was blocked by the Clawvisor proxy") {
		t.Fatalf("missing blocked prompt: %s", out)
	}
	promptIndex, ok := anthropicSSEIndexContaining(out, "Tool use was blocked by the Clawvisor proxy")
	if !ok {
		t.Fatalf("could not find blocked prompt index: %s", out)
	}
	firstToolIndex := -1
	for _, dec := range result.Decisions {
		if firstToolIndex < 0 || dec.ToolUse.Index < firstToolIndex {
			firstToolIndex = dec.ToolUse.Index
		}
	}
	if promptIndex != firstToolIndex {
		t.Fatalf("blocked prompt index=%d must replace first withheld tool index=%d: %s", promptIndex, firstToolIndex, out)
	}
}

func anthropicSSEIndexContaining(stream, needle string) (int, bool) {
	events := strings.Split(strings.ReplaceAll(stream, "\r\n", "\n"), "\n\n")
	for _, event := range events {
		if !strings.Contains(event, needle) {
			continue
		}
		for _, line := range strings.Split(event, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var payload struct {
				Index *int `json:"index"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &payload); err == nil && payload.Index != nil {
				return *payload.Index, true
			}
		}
	}
	return 0, false
}

func TestPostprocessStream_NoStreamingRewriterPassesThrough(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/unknown", nil)
	input := "data: hello\n\n"
	var output bytes.Buffer

	result, err := PostprocessStream(context.Background(), req, strings.NewReader(input), &output, "text/event-stream", PostprocessConfig{
		Inspector: inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
	})
	if err != nil {
		t.Fatalf("PostprocessStream: %v", err)
	}
	if result.SkippedReason == "" {
		t.Fatal("expected skipped reason")
	}
	if output.String() != input {
		t.Fatalf("expected passthrough body %q, got %q", input, output.String())
	}
}

// TestPostprocessStream_FirstTurnNoticeInjectsWithoutInspector covers
// the inspector-disabled pass-through path. The buffered Postprocess
// injects the routing notice independently of inspector state, so the
// streaming path should match for symmetry.
func TestPostprocessStream_FirstTurnNoticeInjectsWithoutInspector(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	input := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_x","role":"assistant","model":"claude-sonnet-4"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"
	var output bytes.Buffer

	result, err := PostprocessStream(context.Background(), req, strings.NewReader(input), &output, "text/event-stream", PostprocessConfig{
		Inspector:       nil,
		FirstTurnNotice: "[Clawvisor] routing notice",
	})
	if err != nil {
		t.Fatalf("PostprocessStream: %v", err)
	}
	if result.SkippedReason != "no inspector configured" {
		t.Fatalf("expected inspector-skipped result; got %q", result.SkippedReason)
	}
	got := output.String()
	if !strings.Contains(got, "[Clawvisor] routing notice") {
		t.Fatalf("notice should surface even without inspector:\n%s", got)
	}
	// Upstream "hi" delta should shift to index 1 since the injected
	// notice occupies index 0.
	if !strings.Contains(got, `"index":1`) {
		t.Errorf("upstream content_block_delta should be shifted to index 1:\n%s", got)
	}
}

// TestPostprocessStream_FirstTurnNoticeSkippedWithoutRewriter covers
// the route-without-streaming-rewriter early return: we can't derive
// a wire shape, so the injector is bypassed and the body passes
// through unchanged.
func TestPostprocessStream_FirstTurnNoticeSkippedWithoutRewriter(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/unknown", nil)
	input := "data: hello\n\n"
	var output bytes.Buffer

	result, err := PostprocessStream(context.Background(), req, strings.NewReader(input), &output, "text/event-stream", PostprocessConfig{
		Inspector:       inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
		FirstTurnNotice: "[Clawvisor] routing notice",
	})
	if err != nil {
		t.Fatalf("PostprocessStream: %v", err)
	}
	if result.SkippedReason == "" {
		t.Fatal("expected skipped reason")
	}
	if output.String() != input {
		t.Fatalf("expected raw passthrough when no rewriter; got %q", output.String())
	}
	if strings.Contains(output.String(), "routing notice") {
		t.Errorf("notice should not surface without a known wire shape:\n%s", output.String())
	}
}

func TestPostprocess_JSONNoTrigger(t *testing.T) {
	body := anthropicJSONWithToolUse(`{"url":"https://example.com/foo"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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

func TestPostprocess_RewriterErrorContinuesWithActionableScriptRecovery(t *testing.T) {
	placeholder := "autovault_google_gmail_eric_clawvisor_com_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	st, userID, agentID := seedPostprocessStoreWithService(t, placeholder, "google.gmail")
	cmd := `python3 <<'PY'
import json, urllib.request

TOKEN = '` + placeholder + `'
mid = 'abc123'
url = f'https://gmail.googleapis.com/gmail/v1/users/me/messages/{mid}?format=metadata'
req = urllib.request.Request(url, headers={'Authorization': f'Bearer {TOKEN}'})
with urllib.request.urlopen(req) as r:
    print(json.load(r))
PY`
	body := anthropicJSONWithNamedToolUse("Bash", `{"command":`+jsonString(cmd)+`,"description":"Fetch Gmail metadata with python"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, postprocessFakeValidator{verdict: inspector.Verdict{
		IsAPICall: true,
		Method:    "GET",
		Host:      "gmail.googleapis.com",
		Path:      "/gmail/v1/users/me/messages/{mid}?format=metadata",
		CredentialLocations: []inspector.CredentialLocation{{
			Kind:   "header",
			Name:   "Authorization",
			Scheme: "Bearer",
		}},
		Reason: "python urllib with autovault bearer",
	}})

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:    insp,
		RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces: NewMemoryCallerNonceCache(time.Minute),
		Store:        st,
		AgentUserID:  userID,
		AgentID:      agentID,
	})

	if !got.Rewritten {
		t.Fatal("unsupported credentialed script should rewrite to a block result")
	}
	if len(got.Decisions) != 1 {
		t.Fatalf("expected one tool decision, got %d", len(got.Decisions))
	}
	reason := got.Decisions[0].Verdict.ContinueWithToolResult
	if reason == "" {
		t.Fatal("rewriter error must populate ContinueWithToolResult for model recovery")
	}
	// Recovery guidance now redirects credentialed-fan-out failures
	// to the autovault script-session path (the supported recovery
	// route for shapes the bash rewriter can't safely parse). The
	// older "emit multiple parallel tool_uses" advice paid the per-
	// call rewriter cost for every request and didn't compose with
	// the script-session machinery the rest of the proxy now relies
	// on; the new message includes the mint endpoint, the verdict-
	// derived host/method/path so the agent can fill the request
	// without guessing, and the docs URL.
	for _, want := range []string{
		"detected credentialed API access",
		"tool shape cannot be rewritten",
		"GET gmail.googleapis.com/gmail/v1/users/me/messages/{mid}?format=metadata",
		"/autovault/script-session",
		"target_host:\"gmail.googleapis.com\"",
		"methods:[\"GET\"]",
		"X-Clawvisor-Caller: Bearer <caller_token>",
		"/autovault/script",
	} {
		if !strings.Contains(reason, want) {
			t.Fatalf("recovery reason missing %q:\n%s", want, reason)
		}
	}
	if strings.Contains(reason, placeholder) {
		t.Fatalf("recovery reason should not echo full placeholder:\n%s", reason)
	}
	if !strings.Contains(string(got.Body), "/autovault/script-session") {
		t.Fatalf("fallback body should include actionable script-session recovery guidance:\n%s", got.Body)
	}
}

func TestPostprocess_AuditsNoTriggerToolUse(t *testing.T) {
	body := anthropicJSONWithToolUse(`{"url":"https://example.com/foo"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
			st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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

func TestPostprocess_SensitiveShellPathRequiresApprovalWithoutPolicyConfig(t *testing.T) {
	body := anthropicJSONWithNamedToolUse("Bash", `{"command":"cat $HOME/.ssh/id_rsa"}`)
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
		RequestID:        "req-bash-sensitive-default",
		PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
		Posture:          runtimedecision.PostureEnforce,
	})
	if !got.Rewritten {
		t.Fatalf("sensitive shell path should require approval even without policy config")
	}
	if text := anthropicResponseText(t, got.Body); !strings.Contains(text, "no matching task scope") {
		t.Fatalf("approval prompt missing scope reason: %s", text)
	}
}

func TestPostprocess_ReadOnlyBashCanBeDisabledByPolicy(t *testing.T) {
	body := anthropicJSONWithNamedToolUse("Bash", `{"command":"ls -la /tmp"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
			st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
			st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
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

// TestPostprocess_SlidingTaskExpiryBumpsOnAuthorizedToolUse guards the
// wiring between EvaluateAuthorization's VerdictAllow and slideTaskExpiry.
// Before this test was added, the slide only fired on the legacy
// cfg.TaskScope.Check fall-through branch; the trigger-miss and
// credentialed paths (which carry CandidateTasks and therefore go through
// EvaluateAuthorization) both returned VerdictAllow without ever bumping
// expires_at, so sliding tasks expired on a fixed window in production.
func TestPostprocess_SlidingTaskExpiryBumpsOnAuthorizedToolUse(t *testing.T) {
	body := anthropicJSONWithNamedToolUse("Write", `{"file_path":"/tmp/hello.py","content":"print('hi')\n"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	// Sliding task with an expiry well inside the slide window so the
	// slide will move expires_at forward to ~now+SlidingTaskSlide.
	soonExpiring := time.Now().UTC().Add(2 * time.Minute)
	task := &store.Task{
		ID:                     "task-slide-1",
		UserID:                 userID,
		AgentID:                agentID,
		Purpose:                "Create /tmp/hello.py",
		Status:                 "active",
		Lifetime:               "sliding",
		ExpiresAt:              &soonExpiring,
		IntentVerificationMode: "off",
		ExpectedTools:          json.RawMessage(`[{"tool_name":"Write","why":"create and refactor /tmp/hello.py"}]`),
	}
	if err := st.CreateTask(req.Context(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

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
		PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
		Posture:          runtimedecision.PostureEnforce,
	})

	if got.Rewritten {
		t.Fatalf("authorized tool_use should pass through, got rewrite: %s", got.Body)
	}

	persisted, err := st.GetTask(req.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if persisted.ExpiresAt == nil {
		t.Fatalf("expires_at should remain non-nil after slide")
	}
	if !persisted.ExpiresAt.After(soonExpiring) {
		t.Fatalf("expires_at should have been bumped forward: was %v, persisted %v",
			soonExpiring, *persisted.ExpiresAt)
	}
	expectedMin := time.Now().UTC().Add(SlidingTaskSlide - 30*time.Second)
	if persisted.ExpiresAt.Before(expectedMin) {
		t.Fatalf("expected expires_at near now+%v, got %v", SlidingTaskSlide, *persisted.ExpiresAt)
	}
}

// TestPostprocess_SessionTaskExpiryDoesNotBumpOnAuthorizedToolUse is the
// negative counterpart: session-lifetime tasks must NOT slide on each
// authorized call. Sliding is opt-in only; session preserves a fixed
// deadline so dashboard-displayed expiries remain stable.
func TestPostprocess_SessionTaskExpiryDoesNotBumpOnAuthorizedToolUse(t *testing.T) {
	body := anthropicJSONWithNamedToolUse("Write", `{"file_path":"/tmp/hello.py","content":"print('hi')\n"}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	soonExpiring := time.Now().UTC().Add(2 * time.Minute)
	task := &store.Task{
		ID:                     "task-session-1",
		UserID:                 userID,
		AgentID:                agentID,
		Purpose:                "Create /tmp/hello.py",
		Status:                 "active",
		Lifetime:               "session",
		ExpiresAt:              &soonExpiring,
		IntentVerificationMode: "off",
		ExpectedTools:          json.RawMessage(`[{"tool_name":"Write","why":"create and refactor /tmp/hello.py"}]`),
	}
	if err := st.CreateTask(req.Context(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		CandidateTasks:   []*store.Task{task},
		ToolRules:        []*store.RuntimePolicyRule{},
		EgressRules:      []*store.RuntimePolicyRule{},
		PendingApprovals: NewMemoryPendingApprovalCache(time.Minute),
		Posture:          runtimedecision.PostureEnforce,
	})

	persisted, err := st.GetTask(req.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if persisted.ExpiresAt == nil {
		t.Fatalf("session expires_at must remain non-nil")
	}
	// sqlite stores expires_at at second precision, so equal-to-the-
	// nanosecond won't hold. Allow ±1 second drift from the original
	// while asserting the slide didn't push it toward now+SlidingTaskSlide.
	delta := persisted.ExpiresAt.Sub(soonExpiring)
	if delta > time.Second || delta < -time.Second {
		t.Fatalf("session expires_at must not move: was %v, persisted %v (delta=%v)", soonExpiring, *persisted.ExpiresAt, delta)
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
	got := taskCreationPromptForHolds([]HeldToolUse{{
		ToolUse: conversation.ToolUse{
			Name:  "Write",
			Input: json.RawMessage(`{"file_path":"/tmp/report.txt","content":"hello"}`),
		},
		Kind: HeldKindApproval,
	}})

	for _, want := range []string{
		"Please request a Clawvisor task",
		"proxy-lite control endpoint",
		"https://clawvisor.local/control/tasks?surface=inline",
		"The user will need to approve the task after it is created",
		"Your next assistant message must be exactly one shell tool_use",
		"Do not print, describe, or summarize the JSON in chat",
		"Do not answer with a markdown code block",
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
	if strings.Contains(got, "```") {
		t.Fatalf("task creation prompt should not wrap the curl in markdown fences:\n%s", got)
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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
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
	// Regression: the coalesced prompt must embed the [clawvisor:approval=<id>]
	// footer for the hold it represents. Without it, a bare "y" reply on the
	// next turn falls back to scanning earlier assistant messages for any
	// marker — picking up a stale inline-task approval and producing
	// approval_not_found / "this approval is no longer valid" when that
	// older hold has already been consumed.
	expectedMarker := "[clawvisor:approval=" + hold.ID + "]"
	if !strings.Contains(out, expectedMarker) {
		t.Fatalf("coalesced prompt must embed approval marker %q so bare-reply release pins to this hold; got: %s", expectedMarker, out)
	}
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
	input := `{"url":"https://api.github.com/repos/x/y/issues","method":"POST","headers":{"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}}`
	body := anthropicJSONWithToolUse(input)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
	input := `{"url":"https://api.github.com/repos/x/y/issues","method":"POST","headers":{"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}}`
	body := anthropicJSONWithToolUse(input)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
		if inputObj.Headers["Authorization"] != "Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" {
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
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"}}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
	if !strings.Contains(out, "Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx") {
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
			 "arguments":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"}}"}
		]
	}`)
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"}}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"name":"WebFetch","arguments":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"}}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"WebFetch","arguments":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"}}"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}

`)
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
	if got := strings.Count(out, "event: response.completed"); got != 1 {
		t.Fatalf("response.completed count=%d, want 1:\n%s", got, out)
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
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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

func TestPostprocessStream_OpenAIResponsesCustomToolCallIsInspectedAndBlocked(t *testing.T) {
	placeholder := "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	st, userID, agentID := seedPostprocessStore(t, placeholder)
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	input := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","status":"in_progress","call_id":"call_custom","name":"WebFetch"}}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","status":"completed","call_id":"call_custom","name":"WebFetch","input":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer ` + placeholder + `\"}}"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
		``,
	}, "\n")

	var output bytes.Buffer
	result, err := PostprocessStream(context.Background(), req, strings.NewReader(input), &output, "text/event-stream", PostprocessConfig{
		Inspector:   inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
	})
	if err != nil {
		t.Fatalf("PostprocessStream: %v", err)
	}
	if len(result.Decisions) != 1 {
		t.Fatalf("expected custom tool call to be inspected once, got %+v", result.Decisions)
	}
	if result.Decisions[0].ToolUse.ID != "call_custom" || result.Decisions[0].ToolUse.Name != "WebFetch" {
		t.Fatalf("unexpected inspected tool use: %+v", result.Decisions[0].ToolUse)
	}
	if !result.Rewritten {
		t.Fatal("expected blocked custom tool call to rewrite stream")
	}
	out := output.String()
	if strings.Contains(out, `"custom_tool_call"`) {
		t.Fatalf("custom tool call leaked before inspection: %s", out)
	}
	if !strings.Contains(out, "Tool use was blocked by the Clawvisor proxy") {
		t.Fatalf("blocked prompt missing: %s", out)
	}
	if got := strings.Count(out, "event: response.completed"); got != 1 {
		t.Fatalf("response.completed count=%d, want 1: %s", got, out)
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
						"arguments":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"}}"
					}
				}]
			},
			"finish_reason":"tool_calls"
		}]
	}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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

data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"WebFetch","arguments":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"}}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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
	input := `{"unknown_field":"autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`
	body := anthropicJSONWithToolUse(input)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

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

func TestPostprocessStream_MidStreamDropReturnsProviderAndResult(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "")

	input := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_test_123","type":"message","role":"assistant","model":"claude-3-opus","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		// Connection lost mid-stream
	}, "\n")

	var output bytes.Buffer
	r := &postprocessErroringReader{data: []byte(input), err: io.ErrUnexpectedEOF}
	result, err := PostprocessStream(context.Background(), req, r, &output, "text/event-stream", PostprocessConfig{
		Inspector:   insp,
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
	})

	if err == nil {
		t.Fatal("expected PostprocessStream to fail due to early EOF")
	}
	if result.StreamingProvider != conversation.ProviderAnthropic {
		t.Errorf("expected StreamingProvider %q, got %q", conversation.ProviderAnthropic, result.StreamingProvider)
	}
	if result.StreamingResult.StreamID != "msg_test_123" {
		t.Errorf("expected StreamID %q, got %q", "msg_test_123", result.StreamingResult.StreamID)
	}
}

func TestWriteStreamError_Anthropic(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	res := conversation.StreamingRewriteResult{
		StreamID:     "msg_123",
		Model:        "claude-3",
		StreamFormat: "anthropic_messages",
	}

	WriteStreamError(&buf, req, conversation.ProviderAnthropic, res, "test error message")
	got := buf.String()

	if !strings.Contains(got, "event: error") {
		t.Errorf("expected event: error, got %q", got)
	}
	if !strings.Contains(got, "test error message") {
		t.Errorf("expected error message in data payload, got %q", got)
	}
	if !strings.Contains(got, `"type":"stream_interrupted"`) {
		t.Errorf("expected stream_interrupted type, got %q", got)
	}
}

func TestWriteStreamError_OpenAIChat(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	res := conversation.StreamingRewriteResult{
		StreamID:     "chatcmpl_123",
		Model:        "gpt-4",
		StreamFormat: "openai_chat",
	}

	WriteStreamError(&buf, req, conversation.ProviderOpenAI, res, "test error message")
	got := buf.String()

	if !strings.Contains(got, "chatcmpl_123") {
		t.Errorf("expected stream id chatcmpl_123, got %q", got)
	}
	if !strings.Contains(got, "gpt-4") {
		t.Errorf("expected model gpt-4, got %q", got)
	}
	if !strings.Contains(got, "test error message") {
		t.Errorf("expected error message, got %q", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Errorf("expected DONE event, got %q", got)
	}
}

func TestWriteStreamError_OpenAIResponses(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	res := conversation.StreamingRewriteResult{
		StreamID:     "resp_123",
		StreamFormat: "openai_responses",
	}

	WriteStreamError(&buf, req, conversation.ProviderOpenAI, res, "test error message")
	got := buf.String()

	if !strings.Contains(got, "event: response.failed") {
		t.Errorf("expected event: response.failed, got %q", got)
	}
	if !strings.Contains(got, "resp_123") {
		t.Errorf("expected stream id resp_123, got %q", got)
	}
	if !strings.Contains(got, "test error message") {
		t.Errorf("expected error message, got %q", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Errorf("expected DONE event, got %q", got)
	}
}

func TestWriteStreamError_Unknown(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	req := httptest.NewRequest("POST", "/v1/unknown", nil)
	res := conversation.StreamingRewriteResult{}

	WriteStreamError(&buf, req, conversation.Provider("unknown"), res, "test\nerror\nmessage")
	got := buf.String()

	want := ": test\n: error\n: message\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

type postprocessErroringReader struct {
	data []byte
	off  int
	err  error
}

func (r *postprocessErroringReader) Read(p []byte) (n int, err error) {
	if r.off >= len(r.data) {
		return 0, r.err
	}
	n = copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

