package conversation

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAnthropicResponseRewriterAllowsToolUseJSON(t *testing.T) {
	t.Parallel()

	body := []byte(`{
	  "id":"msg_1",
	  "type":"message",
	  "role":"assistant",
	  "model":"claude-test",
	  "content":[{"type":"tool_use","id":"toolu_1","name":"fetch_messages","input":{"max_results":10}}],
	  "stop_reason":"tool_use"
	}`)

	result, err := (&AnthropicResponseRewriter{}).Rewrite(body, "application/json", func(tu ToolUse) ToolUseVerdict {
		if tu.Name != "fetch_messages" {
			t.Fatalf("unexpected tool name %q", tu.Name)
		}
		return ToolUseVerdict{Allowed: true}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if result.Rewritten {
		t.Fatal("expected passthrough response")
	}
	if len(result.Decisions) != 1 {
		t.Fatalf("expected one decision, got %d", len(result.Decisions))
	}
	if result.AssistantTurn == nil || !strings.Contains(result.AssistantTurn.Content, "<tool_use name=fetch_messages") {
		t.Fatalf("assistant turn missing tool marker: %+v", result.AssistantTurn)
	}
}

func TestAnthropicResponseRewriterBlocksToolUseJSON(t *testing.T) {
	t.Parallel()

	body := []byte(`{
	  "id":"msg_2",
	  "type":"message",
	  "role":"assistant",
	  "model":"claude-test",
	  "content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"rm -rf /"}}],
	  "stop_reason":"tool_use"
	}`)

	result, err := (&AnthropicResponseRewriter{}).Rewrite(body, "application/json", func(ToolUse) ToolUseVerdict {
		return ToolUseVerdict{
			Allowed:        false,
			Reason:         "requires approval",
			SubstituteWith: "Reply `approve cv-test` to release this tool call.",
		}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected rewritten response")
	}
	var out map[string]any
	if err := json.Unmarshal(result.Body, &out); err != nil {
		t.Fatalf("unmarshal rewritten response: %v", err)
	}
	content := out["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "approve cv-test") {
		t.Fatalf("expected inline approval prompt, got %q", text)
	}
}

func TestAnthropicToolResultIDsFromRequest(t *testing.T) {
	t.Parallel()

	body := []byte(`{
	  "messages":[
	    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"fetch_messages","input":{"max_results":10}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
	  ]
	}`)

	ids := AnthropicToolResultIDsFromRequest(body)
	if len(ids) != 1 || ids[0] != "toolu_1" {
		t.Fatalf("unexpected tool result ids: %v", ids)
	}
}

func TestOpenAIResponseRewriterBlocksResponsesFunctionCallJSON(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := []byte(`{
	  "id":"resp_1",
	  "object":"response",
	  "output":[
	    {"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"Bash","arguments":"{\"command\":\"rm -rf /\"}"}
	  ]
	}`)

	result, err := rewriter.Rewrite(body, "application/json", func(ToolUse) ToolUseVerdict {
		return ToolUseVerdict{
			Allowed:        false,
			Reason:         "requires approval",
			SubstituteWith: "Reply `approve cv-test` to release this tool call.",
		}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected rewritten response")
	}
	var out map[string]any
	if err := json.Unmarshal(result.Body, &out); err != nil {
		t.Fatalf("unmarshal rewritten response: %v", err)
	}
	output := out["output"].([]any)
	text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "approve cv-test") {
		t.Fatalf("expected inline approval prompt, got %q", text)
	}
}

func TestOpenAIResponseRewriterBlocksChatToolCallsSSE(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := []byte(strings.Join([]string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"rm\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))

	result, err := rewriter.Rewrite(body, "text/event-stream", func(tu ToolUse) ToolUseVerdict {
		if tu.Name != "Bash" {
			t.Fatalf("unexpected tool name %q", tu.Name)
		}
		return ToolUseVerdict{Allowed: false, Reason: "requires approval"}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected rewritten SSE response")
	}
	out := string(result.Body)
	if !strings.Contains(out, "Bash: requires approval") {
		t.Fatalf("expected block text in SSE output, got %q", out)
	}
	if strings.Contains(out, `"tool_calls"`) {
		t.Fatalf("blocked tool_calls should not leak into rewritten SSE: %q", out)
	}
}

func TestOpenAIToolResultIDsAndApprovalReply(t *testing.T) {
	t.Parallel()

	responsesBody := []byte(`{
	  "input":[
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"approve cv-abcdef123456"}]},
	    {"type":"function_call_output","call_id":"call_123","output":"ok"}
	  ]
	}`)
	responsesReq, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	verb, id := OpenAIApprovalReply(responsesBody)
	if verb != "approve" || id != "cv-abcdef123456" {
		t.Fatalf("unexpected responses approval reply: verb=%q id=%q", verb, id)
	}
	ids := OpenAIToolResultIDsFromRequest(responsesReq, responsesBody)
	if len(ids) != 1 || ids[0] != "call_123" {
		t.Fatalf("unexpected responses tool result ids: %v", ids)
	}

	chatBody := []byte(`{
	  "messages":[
	    {"role":"user","content":"deny"},
	    {"role":"tool","tool_call_id":"call_456","content":"error"}
	  ]
	}`)
	chatReq, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	verb, id = OpenAIApprovalReply(chatBody)
	if verb != "deny" || id != "" {
		t.Fatalf("unexpected chat approval reply: verb=%q id=%q", verb, id)
	}
	ids = OpenAIToolResultIDsFromRequest(chatReq, chatBody)
	if len(ids) != 1 || ids[0] != "call_456" {
		t.Fatalf("unexpected chat tool result ids: %v", ids)
	}
}

func TestApplyBlockSubstitutionsMatchesToolDecisionsByPosition(t *testing.T) {
	t.Parallel()

	frags := []assistantFragment{
		{IsTool: true, ToolName: "Bash", ToolArgs: json.RawMessage(`{"command":"pwd"}`)},
		{IsTool: true, ToolName: "Bash", ToolArgs: json.RawMessage(`{"command":"rm -rf /tmp/demo"}`)},
	}
	decisions := []ToolUseDecisionRecord{
		{ToolUse: ToolUse{Name: "Bash"}, Verdict: ToolUseVerdict{Allowed: true}},
		{ToolUse: ToolUse{Name: "Bash"}, Verdict: ToolUseVerdict{Allowed: false, Reason: "requires approval"}},
	}

	got := applyBlockSubstitutions(frags, decisions)
	if len(got) != 2 {
		t.Fatalf("expected two fragments, got %d", len(got))
	}
	if !got[0].IsTool || got[0].ToolName != "Bash" {
		t.Fatalf("expected first Bash tool fragment to remain allowed, got %+v", got[0])
	}
	if got[1].IsTool || !strings.Contains(got[1].Text, "requires approval") {
		t.Fatalf("expected second Bash tool fragment to be substituted, got %+v", got[1])
	}
}

func TestOpenAIResponseRewriterSortsStreamingChatToolCallsByIndex(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := []byte(strings.Join([]string{
		`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"second","arguments":"{\"step\":2}"}},{"index":0,"id":"call_1","type":"function","function":{"name":"first","arguments":"{\"step\":1}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))

	var seen []string
	result, err := rewriter.Rewrite(body, "text/event-stream", func(tu ToolUse) ToolUseVerdict {
		seen = append(seen, tu.Name)
		return ToolUseVerdict{Allowed: false, Reason: tu.Name}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if len(seen) != 2 || seen[0] != "first" || seen[1] != "second" {
		t.Fatalf("expected deterministic tool-call order [first second], got %v", seen)
	}
	if len(result.Decisions) != 2 || result.Decisions[0].ToolUse.Index != 0 || result.Decisions[1].ToolUse.Index != 1 {
		t.Fatalf("unexpected decision indexes: %+v", result.Decisions)
	}
}
