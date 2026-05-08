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
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

// seedPostprocessStore returns a store with a github placeholder + agent
// owned by `userID/agentID`. Tests that rely on the boundary check pass
// the placeholder string into their tool_use input.
func seedPostprocessStore(t *testing.T, placeholder string) (store.Store, string, string) {
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
		ServiceID:   "github",
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}
	return st, user.ID, agent.ID
}

func anthropicJSONWithToolUse(input string) []byte {
	return []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"text","text":"sure"},
			{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":` + input + `}
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
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
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
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
		Audit:       NewAuditEmitter(st, nil, nil),
		RequestID:   "req-audit",
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
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
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

func TestPostprocess_HoldsMultipleApprovalPromptsInOneResponse(t *testing.T) {
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
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
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
		t.Fatalf("expected approval prompts for reviewed tool calls")
	}
	first, err := cache.Resolve(req.Context(), ResolveRequest{UserID: userID, AgentID: agentID, Provider: conversation.ProviderAnthropic})
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.ToolUse.ID != "toolu_1" {
		t.Fatalf("first resolved pending = %+v, want toolu_1", first)
	}
	second, err := cache.Resolve(req.Context(), ResolveRequest{UserID: userID, AgentID: agentID, Provider: conversation.ProviderAnthropic})
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.ToolUse.ID != "toolu_2" {
		t.Fatalf("second resolved pending = %+v, want toolu_2", second)
	}
}

func TestPostprocess_ObservePostureDoesNotBlockToolDenyRule(t *testing.T) {
	input := `{"url":"https://api.github.com/repos/x/y/issues","method":"POST","headers":{"Authorization":"Bearer autovault_github_xxx"}}`
	body := anthropicJSONWithToolUse(input)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
		Posture:     runtimedecision.PostureObserve,
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
	if !strings.Contains(string(got.Body), "https://proxy.example/proxy/v1/repos/x/y/issues") {
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
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
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
		if !strings.HasPrefix(inputObj.URL, "https://proxy.example/proxy/v1/repos/x/y/issues") {
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
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
	})

	if !got.Rewritten {
		t.Fatalf("expected SSE rewrite to fire on autovault placeholder")
	}
	out := string(got.Body)
	if !strings.Contains(out, "https://proxy.example/proxy/v1/repos/x/y/issues") {
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
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
	})

	if !got.Rewritten {
		t.Fatalf("expected rewrite for OpenAI Responses JSON, got skipped=%q", got.SkippedReason)
	}
	if !strings.Contains(string(got.Body), "https://proxy.example/proxy/v1/repos/x/y/issues") {
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
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
	})

	if !got.Rewritten {
		t.Fatalf("expected SSE rewrite for OpenAI Responses, got skipped=%q", got.SkippedReason)
	}
	out := string(got.Body)
	if !strings.Contains(out, "https://proxy.example/proxy/v1/repos/x/y/issues") {
		t.Fatalf("rewritten URL missing:\n%s", out)
	}
	if !strings.Contains(out, "response.output_item.done") || !strings.Contains(out, "response.completed") {
		t.Fatalf("Responses SSE envelope missing:\n%s", out)
	}
	if !strings.Contains(out, "function_call_arguments.done") {
		t.Fatalf("function_call_arguments.done missing — Codex needs this signal:\n%s", out)
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
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
	})

	if !got.Rewritten {
		t.Fatalf("expected rewrite for OpenAI Chat JSON, got skipped=%q", got.SkippedReason)
	}
	if !strings.Contains(string(got.Body), "https://proxy.example/proxy/v1/repos/x/y/issues") {
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
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
	})

	if !got.Rewritten {
		t.Fatalf("expected SSE rewrite for OpenAI Chat, got skipped=%q", got.SkippedReason)
	}
	out := string(got.Body)
	if !strings.Contains(out, "https://proxy.example/proxy/v1/repos/x/y/issues") {
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
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
	})

	// "Block" path of the existing rewriter replaces the content with text.
	if !got.Rewritten {
		t.Fatalf("expected rewrite-to-blocked when ambiguous")
	}
	if !strings.Contains(string(got.Body), "Clawvisor") {
		t.Fatalf("expected blocked-explanation text, got %q", string(got.Body))
	}
}
