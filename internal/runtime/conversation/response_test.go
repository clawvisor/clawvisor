package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSyntheticApprovalToolUseResponseOpenAIChatLiteProxyRoute(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/chat/completions", nil)
	body := []byte(`{"messages":[{"role":"user","content":"approve"}]}`)
	resp, ok := SyntheticApprovalToolUseResponse(req, ProviderOpenAI, body, true, "call_123", "write", map[string]any{"path": "index.html"})
	if !ok {
		t.Fatal("expected synthetic response")
	}
	if !strings.Contains(string(resp.Body), `"object":"chat.completion"`) {
		t.Fatalf("expected chat completions response for lite-proxy route, got %s", resp.Body)
	}
	if !strings.Contains(string(resp.Body), `"tool_calls"`) {
		t.Fatalf("expected chat tool_calls response, got %s", resp.Body)
	}
}

func TestResponseRegistryForProviderStreamingHandlesMissingProvider(t *testing.T) {
	t.Parallel()

	if got := DefaultResponseRegistry().ForProviderStreaming(ProviderAnthropic); got == nil {
		t.Fatal("registered Anthropic streaming rewriter is nil")
	}
	if got := DefaultResponseRegistry().ForProviderStreaming(ProviderOpenAI); got == nil {
		t.Fatal("registered OpenAI streaming rewriter is nil")
	}
	if got := DefaultResponseRegistry().ForProviderStreaming(Provider("missing")); got != nil {
		t.Fatalf("missing provider streaming rewriter = %#v, want nil", got)
	}
}

func TestBlockedReasonTextPreservesEmptyContract(t *testing.T) {
	t.Parallel()

	if got := BlockedReasonText(nil); got != "" {
		t.Fatalf("BlockedReasonText(nil)=%q, want empty", got)
	}
	if got := blockedReasonTextForAssistant(nil); strings.TrimSpace(got) == "" {
		t.Fatal("assistant blocked-reason fallback returned empty text")
	}
}

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
			SubstituteWith: "Reply `approve` to run it or `deny` to block it.",
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
	if !strings.Contains(text, "Reply `approve`") {
		t.Fatalf("expected inline approval prompt, got %q", text)
	}
}

func TestAnthropicResponseRewriterOmitsEmptyTextBlocksWhenRewritingSSE(t *testing.T) {
	t.Parallel()

	body := []byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"url\":\"https://clawvisor.local/control/skill\",\"prompt\":\"What is here?\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`)

	result, err := (&AnthropicResponseRewriter{}).Rewrite(body, "text/event-stream", func(tu ToolUse) ToolUseVerdict {
		return ToolUseVerdict{
			Allowed:      true,
			RewriteInput: json.RawMessage(`{"url":"https://example.test/api/control/skill","prompt":"What is here?"}`),
		}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected rewritten SSE")
	}
	out := string(result.Body)
	if strings.Contains(out, `"type":"text","text":""`) {
		t.Fatalf("rewritten SSE should not include an empty text block: %s", out)
	}
	if !strings.Contains(out, `"index":0`) || strings.Contains(out, `"index":1`) {
		t.Fatalf("rewritten SSE should reindex the remaining tool block to 0: %s", out)
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
			SubstituteWith: "Reply `approve` to run it or `deny` to block it.",
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
	if !strings.Contains(text, "Reply `approve`") {
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

func TestOpenAIResponseRewriterBlocksResponsesFunctionCallSSE(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := SynthOpenAIResponsesFunctionCallSSE("call_1", "exec_command", map[string]any{
		"cmd": "cat /tmp/hello_world.sh",
	})
	result, err := rewriter.Rewrite(body, "text/event-stream", func(ToolUse) ToolUseVerdict {
		return ToolUseVerdict{
			Allowed:        false,
			Reason:         "Ask before running exec_command",
			SubstituteWith: "Clawvisor paused this tool call for approval.\n\nReply `approve` to run it or `deny` to block it.",
		}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected rewritten SSE response")
	}
	out := string(result.Body)
	if !strings.Contains(out, "Reply `approve`") {
		t.Fatalf("expected inline approval prompt in SSE output, got %q", out)
	}
	if !strings.Contains(out, `"output_text":"Clawvisor paused this tool call`) {
		t.Fatalf("expected final response.completed output_text for Codex clients, got %q", out)
	}
	if !strings.Contains(out, `"content":[{"text":"Clawvisor paused this tool call`) {
		t.Fatalf("expected completed message item content for Codex clients, got %q", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("expected [DONE] terminator, got %q", out)
	}
}

func TestOpenAIToolResultIDsAndApprovalReply(t *testing.T) {
	t.Parallel()

	// function_call_output must precede the user approval reply: tool
	// outputs only appear after a prior release, so a real conversation
	// can't have one sitting AFTER the message that's now requesting a
	// release. OpenAIApprovalReply treats a function_call_output after
	// the latest user message as a signal the approval already happened.
	responsesBody := []byte(`{
	  "input":[
	    {"type":"function_call_output","call_id":"call_123","output":"ok"},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"approve cv-abcdefghijklmnopqrstuvwxyz"}]}
	  ]
	}`)
	responsesReq, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	verb, id := OpenAIApprovalReply(responsesBody)
	if verb != "approve" || id != "cv-abcdefghijklmnopqrstuvwxyz" {
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

	wrappedBody := []byte(`{
	  "messages":[
	    {"role":"user","content":"Conversation info:\njson:{\"chat_id\":\"telegram:123\"}\n\napprove"}
	  ]
	}`)
	verb, id = OpenAIApprovalReply(wrappedBody)
	if verb != "approve" || id != "" {
		t.Fatalf("unexpected wrapped approval reply: verb=%q id=%q", verb, id)
	}

	trailingBody := []byte(`{
	  "messages":[
	    {"role":"user","content":"approve\n\nthanks"}
	  ]
	}`)
	verb, id = OpenAIApprovalReply(trailingBody)
	if verb != "approve" || id != "" {
		t.Fatalf("unexpected trailing approval reply: verb=%q id=%q", verb, id)
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

// OpenAI Chat streams can interleave assistant prose with tool_calls.
// When the rewriter mutates the tool_call arguments, the synthesized
// re-emit must preserve the leading text. Previously the text buffer
// was reset on finish_reason="tool_calls" and the re-emit was built
// from the empty buffer, silently dropping the prose.
func TestOpenAIResponseRewriterPreservesLeadingTextOnChatRewrite(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := []byte(strings.Join([]string{
		`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Looking up your repo. "},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"github","arguments":"{\"repo\":\"acme\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))

	result, err := rewriter.Rewrite(body, "text/event-stream", func(tu ToolUse) ToolUseVerdict {
		// Rewrite the tool_call arguments so the rewrite path fires.
		return ToolUseVerdict{Allowed: true, RewriteInput: []byte(`{"repo":"acme/rewritten"}`)}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatalf("expected rewrite to fire")
	}
	if !strings.Contains(string(result.Body), "Looking up your repo.") {
		t.Fatalf("leading prose dropped after rewrite:\n%s", result.Body)
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

func TestAnthropicStreamRewritePreservesIndices(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Thinking process..."}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"url\":\"https://example.test\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var output bytes.Buffer
	rewriter := AnthropicResponseRewriter{}
	res, err := rewriter.StreamRewrite(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatalf("StreamRewrite failed: %v", err)
	}

	if len(res.ToolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(res.ToolUses))
	}

	tu := res.ToolUses[0]
	if tu.Index != 1 {
		t.Errorf("expected tool use index to be 1, got %d", tu.Index)
	}
	if tu.Name != "WebFetch" {
		t.Errorf("expected tool name WebFetch, got %q", tu.Name)
	}

	outStr := output.String()
	if !strings.Contains(outStr, "Thinking process...") {
		t.Errorf("expected output to contain text prose, got: %q", outStr)
	}
	if strings.Contains(outStr, "WebFetch") {
		t.Errorf("expected output to buffer/exclude the tool use, but got: %q", outStr)
	}
	assertAnthropicStreamHasOnlyTextBlock(t, outStr, 0)
}

func TestAnthropicStreamRewriteTextOnlyPreservesEndTurn(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_text","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var output bytes.Buffer
	res, err := (AnthropicResponseRewriter{}).StreamRewrite(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatalf("StreamRewrite: %v", err)
	}
	if len(res.ToolUses) != 0 {
		t.Fatalf("expected no tool uses, got %d", len(res.ToolUses))
	}
	out := output.String()
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Fatalf("text-only stream lost original end_turn stop: %s", out)
	}
	if strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("text-only stream must not synthesize tool_use stop: %s", out)
	}
	if res.NextAnthropicContentIndex != 1 {
		t.Fatalf("NextAnthropicContentIndex=%d, want 1", res.NextAnthropicContentIndex)
	}
}

func TestAnthropicStreamRewritePreservesThinkingBlocks(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_text","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[]}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"initial thought","signature":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"deep thought"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig123"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Visible"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var output bytes.Buffer
	res, err := (AnthropicResponseRewriter{}).StreamRewrite(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatalf("StreamRewrite: %v", err)
	}
	out := output.String()
	// Thinking blocks should now be preserved in the output.
	if !strings.Contains(out, `"thinking"`) {
		t.Fatalf("thinking block should be preserved in SSE output: %s", out)
	}
	if !strings.Contains(out, "deep thought") {
		t.Fatalf("thinking delta content should be streamed: %s", out)
	}
	if got := fmt.Sprint(res.AssistantTurn.Content); !strings.Contains(got, "initial thought") {
		t.Fatalf("assistant turn should retain thinking content from block start, got %v", res.AssistantTurn.Content)
	}
	if !strings.Contains(out, "sig123") {
		t.Fatalf("signature delta should be streamed: %s", out)
	}
	if !strings.Contains(out, "Visible") {
		t.Fatalf("text block should be preserved: %s", out)
	}
	// Both blocks should be present with sequential indices 0 and 1.
	if !strings.Contains(out, `"index":0`) || !strings.Contains(out, `"index":1`) {
		t.Fatalf("both blocks should have sequential indices: %s", out)
	}
	if res.NextAnthropicContentIndex != 2 {
		t.Fatalf("NextAnthropicContentIndex=%d, want 2", res.NextAnthropicContentIndex)
	}
}

func TestAnthropicStreamRewriteDropsNonClaudeThinkingBlocks(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_text","type":"message","role":"assistant","model":"openai/gpt-oss-120b:free","content":[]}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hidden thought"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Visible"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var output bytes.Buffer
	res, err := (AnthropicResponseRewriter{}).StreamRewrite(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatalf("StreamRewrite: %v", err)
	}
	out := output.String()
	if strings.Contains(out, "thinking") || strings.Contains(out, "hidden thought") {
		t.Fatalf("non-Claude thinking block should be filtered from SSE output: %s", out)
	}
	if !strings.Contains(out, "Visible") {
		t.Fatalf("text block should be preserved: %s", out)
	}
	if !strings.Contains(out, `"index":0`) {
		t.Fatalf("first visible block should be reindexed to 0: %s", out)
	}
	assertNoAnthropicSSEIndex(t, out, 1)
	if res.NextAnthropicContentIndex != 1 {
		t.Fatalf("NextAnthropicContentIndex=%d, want 1", res.NextAnthropicContentIndex)
	}
	if got := fmt.Sprint(res.AssistantTurn.Content); strings.Contains(got, "hidden thought") || !strings.Contains(got, "Visible") {
		t.Fatalf("assistant turn should include visible text only, got %v", res.AssistantTurn.Content)
	}
}

func TestAnthropicStreamRewriteKeepsThinkingForClaudeOnlyNames(t *testing.T) {
	t.Parallel()

	if isNonClaudeModel("anthropic/claude-3-5-sonnet") {
		t.Fatal("provider-qualified claude model should be treated as Claude")
	}
	if !isNonClaudeModel("myclaude-clone") {
		t.Fatal("myclaude-clone must not be treated as Claude")
	}
}

func TestAnthropicStreamRewriteStopsOnContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	_, err := (AnthropicResponseRewriter{}).StreamRewrite(ctx, strings.NewReader("event: message_start\ndata: {}\n\n"), &output)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestAnthropicStreamRewriteStopsOnMidStreamCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		var output bytes.Buffer
		_, err := (AnthropicResponseRewriter{}).StreamRewrite(ctx, pr, &output)
		errCh <- err
	}()

	_, _ = pw.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"claude-test\"}}\n\n"))
	cancel()
	_ = pw.Close()

	if err := <-errCh; err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestAnthropicStreamRewriteNextIndexAfterMultipleTextBlocks(t *testing.T) {
	t.Parallel()

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
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":2}`,
		``,
	}, "\n")

	var output bytes.Buffer
	res, err := (AnthropicResponseRewriter{}).StreamRewrite(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatalf("StreamRewrite: %v", err)
	}
	if len(res.ToolUses) != 1 || res.ToolUses[0].Index != 2 {
		t.Fatalf("unexpected tool uses: %+v", res.ToolUses)
	}
	if res.NextAnthropicContentIndex != 2 {
		t.Fatalf("NextAnthropicContentIndex=%d, want 2", res.NextAnthropicContentIndex)
	}
}

func TestAnthropicStreamRewriteBuffersContentAfterToolUse(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[]}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Before"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":"After"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":2}`,
		``,
	}, "\n")

	var output bytes.Buffer
	res, err := (AnthropicResponseRewriter{}).StreamRewrite(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatalf("StreamRewrite: %v", err)
	}
	if len(res.ToolUses) != 1 || res.ToolUses[0].Index != 1 {
		t.Fatalf("unexpected tool indexes: %+v", res.ToolUses)
	}
	if out := output.String(); strings.Contains(out, "After") || !strings.Contains(out, "Before") {
		t.Fatalf("stream should include only text before first withheld tool, got %s", out)
	}
	if got := fmt.Sprint(res.AssistantTurn.Content); !strings.Contains(got, "After") {
		t.Fatalf("assistant turn should retain buffered post-tool text, got %v", res.AssistantTurn.Content)
	}
}

func TestOpenAIChatStreamRewriteTextOnlyPreservesStopAndDone(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`data: {"id":"chatcmpl_text","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_text","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var output bytes.Buffer
	res, err := (OpenAIResponseRewriter{}).StreamRewrite(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatalf("StreamRewrite: %v", err)
	}
	if len(res.ToolUses) != 0 {
		t.Fatalf("expected no tool uses, got %d", len(res.ToolUses))
	}
	out := output.String()
	if !strings.Contains(out, `"content":"Hello"`) {
		t.Fatalf("text-only chat stream lost assistant content: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) || !strings.Contains(out, `data: [DONE]`) {
		t.Fatalf("text-only chat stream lost stop or DONE: %s", out)
	}
	if strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("text-only chat stream must not synthesize tool_calls finish: %s", out)
	}
}

func TestOpenAIResponsesStreamRewriteTextOnlyPreservesCompleted(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"Hello"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
		``,
	}, "\n")

	var output bytes.Buffer
	res, err := (OpenAIResponseRewriter{}).StreamRewrite(context.Background(), strings.NewReader(input), &output)
	if err != nil {
		t.Fatalf("StreamRewrite: %v", err)
	}
	if len(res.ToolUses) != 0 {
		t.Fatalf("expected no tool uses, got %d", len(res.ToolUses))
	}
	if out := output.String(); !strings.Contains(out, "event: response.completed") {
		t.Fatalf("text-only responses stream lost response.completed: %s", out)
	}
}

func assertNoAnthropicSSEIndex(t *testing.T, stream string, forbidden int) {
	t.Helper()
	for _, event := range parseTestSSEEvents(t, stream) {
		switch event.Event {
		case "content_block_start", "content_block_delta", "content_block_stop":
		default:
			continue
		}
		var payload struct {
			Index *int `json:"index"`
		}
		if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
			t.Fatalf("malformed SSE event data in %q: %v", event.Event, err)
		}
		if payload.Index != nil && *payload.Index == forbidden {
			t.Fatalf("stream contains forbidden index %d in event %q: %s", forbidden, event.Event, stream)
		}
	}
}

func assertAnthropicStreamHasOnlyTextBlock(t *testing.T, stream string, wantIndex int) {
	t.Helper()
	sawStart, sawDelta, sawStop := false, false, false
	for _, event := range parseTestSSEEvents(t, stream) {
		switch event.Event {
		case "content_block_start":
			var payload struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
				t.Fatalf("parse content_block_start: %v", err)
			}
			if payload.Index != wantIndex || payload.ContentBlock.Type != "text" {
				t.Fatalf("unexpected content_block_start: %+v", payload)
			}
			sawStart = true
		case "content_block_delta":
			var payload struct {
				Index int `json:"index"`
				Delta struct {
					Type string `json:"type"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
				t.Fatalf("parse content_block_delta: %v", err)
			}
			if payload.Index != wantIndex || payload.Delta.Type != "text_delta" {
				t.Fatalf("unexpected content_block_delta: %+v", payload)
			}
			sawDelta = true
		case "content_block_stop":
			var payload struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
				t.Fatalf("parse content_block_stop: %v", err)
			}
			if payload.Index != wantIndex {
				t.Fatalf("unexpected content_block_stop: %+v", payload)
			}
			sawStop = true
		}
	}
	if !sawStart || !sawStop {
		t.Fatalf("missing text block events: start=%v delta=%v stop=%v stream=%s", sawStart, sawDelta, sawStop, stream)
	}
}

func parseTestSSEEvents(t *testing.T, stream string) []sseEvent {
	t.Helper()
	events, err := parseSSEEvents([]byte(stream))
	if err != nil {
		t.Fatalf("parse SSE events: %v", err)
	}
	return events
}
