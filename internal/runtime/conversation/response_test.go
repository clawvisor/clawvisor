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
	if !strings.Contains(out, "Tool 'Bash' was blocked by Clawvisor policy: requires approval") {
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

func TestOpenAIResponseRewriterPreservesPostToolTextOnChatRewrite(t *testing.T) {
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
		`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"And here is some trailing text."},"finish_reason":null}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))

	result, err := rewriter.Rewrite(body, "text/event-stream", func(tu ToolUse) ToolUseVerdict {
		return ToolUseVerdict{Allowed: true, RewriteInput: []byte(`{"repo":"acme/rewritten"}`)}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatalf("expected rewrite to fire")
	}

	events := parseTestSSEEvents(t, string(result.Body))
	var eventOrder []string
	for _, ev := range events {
		var payload struct {
			Choices []struct {
				Delta struct {
					Content   any   `json:"content"`
					ToolCalls []any `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			continue
		}
		for _, c := range payload.Choices {
			if txt := flattenOpenAIContentFromAny(c.Delta.Content); txt != "" {
				eventOrder = append(eventOrder, txt)
			}
			if len(c.Delta.ToolCalls) > 0 {
				eventOrder = append(eventOrder, "tool_calls")
			}
		}
	}

	// We expect precisely: ["Looking up your repo. ", "tool_calls", "And here is some trailing text."]
	expected := []string{"Looking up your repo. ", "tool_calls", "And here is some trailing text."}
	if len(eventOrder) != len(expected) {
		t.Fatalf("expected sequence %v, got %v", expected, eventOrder)
	}
	for i, val := range expected {
		if eventOrder[i] != val {
			t.Fatalf("at index %d: expected %q, got %q", i, val, eventOrder[i])
		}
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

// Regression: a thinking_delta event with an empty `thinking` field must
// be passed through with the field intact. Previously the rewriter
// round-tripped the delta through a typed struct where `Thinking` had
// `,omitempty`, so an empty value was dropped on re-marshal. Claude
// Code's harness reads `delta.thinking` unconditionally on
// thinking_delta events; a missing field reads as `undefined`, which
// JS coerces to the literal string "undefined" and concatenates into
// the stored thinking text — corrupting the assistant turn and
// triggering Anthropic's "thinking blocks cannot be modified" 400 on
// the next request that includes the turn.
func TestAnthropicStreamRewritePreservesEmptyThinkingDeltaField(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_x","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[]}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`,
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
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}`,
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
	if _, err := (AnthropicResponseRewriter{}).StreamRewrite(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatalf("StreamRewrite: %v", err)
	}
	out := output.String()

	if !strings.Contains(out, `"type":"thinking_delta","thinking":""`) {
		t.Fatalf("thinking_delta should retain explicit empty `thinking` field, got:\n%s", out)
	}
}

// Anthropic enforces a cryptographic signature over thinking blocks
// across turns: any reshape of the assistant turn — reordered keys,
// dropped unknown fields, stripped whitespace — corrupts the
// downstream verification and causes 400s with "thinking or
// redacted_thinking blocks ... cannot be modified".
//
// This test pins byte fidelity for thinking-related events through
// StreamRewrite when no index shift is needed: the upstream bytes
// must come through unchanged, key order preserved, including fields
// the rewriter does not model (e.g. estimated_tokens) and trailing
// SSE whitespace.
func TestAnthropicStreamRewriteThinkingEventsAreBytePreserved(t *testing.T) {
	t.Parallel()

	// Mimic the exact byte shape Anthropic emits for an opus thinking
	// stream — including the `estimated_tokens` field the rewriter
	// has no struct field for, and trailing whitespace on the data
	// payload that an inadvertent re-marshal would strip.
	upstreamEvents := []struct{ event, data string }{
		{"message_start", `{"type":"message_start","message":{"id":"msg_x","type":"message","role":"assistant","model":"claude-opus-4-7","content":[]}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"","estimated_tokens":50}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc123"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}

	var sb strings.Builder
	for _, ev := range upstreamEvents {
		sb.WriteString("event: ")
		sb.WriteString(ev.event)
		sb.WriteString("\ndata: ")
		sb.WriteString(ev.data)
		sb.WriteString("\n\n")
	}
	input := sb.String()

	var output bytes.Buffer
	if _, err := (AnthropicResponseRewriter{}).StreamRewrite(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatalf("StreamRewrite: %v", err)
	}
	out := output.String()

	// Each upstream event's data line must appear verbatim in the
	// output — same key order, same trailing whitespace, same
	// unmodelled fields.
	for _, ev := range upstreamEvents {
		expected := "data: " + ev.data + "\n\n"
		if !strings.Contains(out, expected) {
			t.Errorf("event %q data not byte-preserved.\n  want: %q\n  output: %s", ev.event, ev.data, out)
		}
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

func TestOpenAIResponseRewriterPreservesMultimodalChatJSON(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := []byte(`{
	  "id": "chatcmpl_multimodal",
	  "choices": [{
	    "index": 0,
	    "message": {
	      "role": "assistant",
	      "content": [
	        {"type": "text", "text": "Here is the image you asked for."},
	        {"type": "image_url", "image_url": {"url": "https://example.com/image.png"}}
	      ],
	      "tool_calls": [
	        {"id": "call_1", "type": "function", "function": {"name": "Bash", "arguments": "{\"command\":\"rm\"}"}}
	      ]
	    }
	  }]
	}`)

	result, err := rewriter.Rewrite(body, "application/json", func(tu ToolUse) ToolUseVerdict {
		return ToolUseVerdict{Allowed: false, Reason: "requires approval"}
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
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	content := msg["content"].([]any)

	if len(content) != 3 {
		t.Fatalf("expected 3 content parts, got %d: %+v", len(content), content)
	}
	if content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "Here is the image you asked for." {
		t.Fatalf("unexpected first part: %+v", content[0])
	}
	if content[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("unexpected second part (image_url): %+v", content[1])
	}
	if content[2].(map[string]any)["type"] != "text" || !strings.Contains(content[2].(map[string]any)["text"].(string), "requires approval") {
		t.Fatalf("unexpected third part (blocked prompt): %+v", content[2])
	}
}

func TestOpenAIResponseRewriterClearsStaleOutputTextResponsesJSON(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := []byte(`{
	  "id": "resp_stale",
	  "object": "response",
	  "output": [
	    {"id": "fc_1", "type": "function_call", "status": "completed", "call_id": "call_1", "name": "Bash", "arguments": "{\"command\":\"rm\"}"}
	  ],
	  "output_text": "I am about to run a command"
	}`)

	result, err := rewriter.Rewrite(body, "application/json", func(tu ToolUse) ToolUseVerdict {
		return ToolUseVerdict{Allowed: true, RewriteInput: []byte(`{"command":"ls"}`)}
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
	outputText, _ := out["output_text"].(string)
	if outputText != "" {
		t.Fatalf("expected output_text to be cleared/empty, got %q", outputText)
	}
}

func TestOpenAIResponseRewriterDoesNotMutateUnrelatedChoicesChatJSON(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := []byte(`{
	  "id": "chatcmpl_multi_choice",
	  "choices": [
	    {
	      "index": 0,
	      "message": {
	        "role": "assistant",
	        "content": "Choice 0",
	        "tool_calls": [
	          {"id": "call_1", "type": "function", "function": {"name": "Bash", "arguments": "{\"command\":\"rm\"}"}}
	        ]
	      },
	      "finish_reason": "tool_calls"
	    },
	    {
	      "index": 1,
	      "message": {
	        "role": "assistant",
	        "content": "Choice 1",
	        "tool_calls": [
	          {"id": "call_2", "type": "function", "function": {"name": "WebFetch", "arguments": "{\"url\":\"https://safe.test\"}"}}
	        ]
	      },
	      "finish_reason": "tool_calls"
	    }
	  ]
	}`)

	// Only block Choice 0 (the Bash tool), keep Choice 1 (WebFetch) allowed
	result, err := rewriter.Rewrite(body, "application/json", func(tu ToolUse) ToolUseVerdict {
		if tu.Name == "Bash" {
			return ToolUseVerdict{Allowed: false, Reason: "requires approval"}
		}
		return ToolUseVerdict{Allowed: true}
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
	choices := out["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("expected 2 choices, got %d", len(choices))
	}

	// Choice 0 should be rewritten (Bash tool call blocked/removed, content modified)
	c0 := choices[0].(map[string]any)
	c0Msg := c0["message"].(map[string]any)
	c0Calls, _ := c0Msg["tool_calls"].([]any)
	if len(c0Calls) != 0 {
		t.Fatalf("expected Choice 0 tool_calls to be removed, got %v", c0Calls)
	}
	c0Content := c0Msg["content"].(string)
	if !strings.Contains(c0Content, "requires approval") {
		t.Fatalf("expected Choice 0 content to contain block message, got %q", c0Content)
	}
	if c0["finish_reason"] != "stop" {
		t.Fatalf("expected Choice 0 finish_reason to be stop, got %q", c0["finish_reason"])
	}

	// Choice 1 should remain untouched (WebFetch tool call allowed and intact)
	c1 := choices[1].(map[string]any)
	c1Msg := c1["message"].(map[string]any)
	c1Calls, _ := c1Msg["tool_calls"].([]any)
	if len(c1Calls) != 1 {
		t.Fatalf("expected Choice 1 tool_calls to be preserved, got %v", c1Calls)
	}
	c1Content := c1Msg["content"].(string)
	if c1Content != "Choice 1" {
		t.Fatalf("expected Choice 1 content to be Choice 1, got %q", c1Content)
	}
	if c1["finish_reason"] != "tool_calls" {
		t.Fatalf("expected Choice 1 finish_reason to be tool_calls, got %q", c1["finish_reason"])
	}
}

func TestOpenAIResponseRewriterDoesNotMutateUnrelatedChoicesChatSSE(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := []byte(strings.Join([]string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null},{"index":1,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"rm\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"call_2","type":"function","function":{"name":"WebFetch","arguments":"{\"url\":\"https://safe.test\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":1,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))

	// Block Bash on Choice 0, allow WebFetch on Choice 1
	result, err := rewriter.Rewrite(body, "text/event-stream", func(tu ToolUse) ToolUseVerdict {
		if tu.Name == "Bash" {
			return ToolUseVerdict{Allowed: false, Reason: "requires approval"}
		}
		return ToolUseVerdict{Allowed: true}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected rewritten response")
	}

	t.Logf("rewritten body:\n%s", string(result.Body))
	events := parseTestSSEEvents(t, string(result.Body))
	
	// We expect the rewritten events to synthesize both choices separately.
	// Let's verify that the choice 0 gets finish_reason stop/blocked content,
	// while choice 1 gets finish_reason tool_calls with its tool call preserved.
	var choice0Finished, choice1Finished bool
	var choice0HasContent, choice0HasToolCalls bool
	var choice1HasContent, choice1HasToolCalls bool

	for _, ev := range events {
		var payload struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content   any   `json:"content"`
					ToolCalls []any `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			continue
		}
		for _, c := range payload.Choices {
			if c.Index == 0 {
				if c.Delta.Content != nil {
					choice0HasContent = true
					txt := flattenOpenAIContentFromAny(c.Delta.Content)
					if !strings.Contains(txt, "requires approval") {
						t.Fatalf("expected Choice 0 content to contain block message, got %q", txt)
					}
				}
				if len(c.Delta.ToolCalls) > 0 {
					choice0HasToolCalls = true
				}
				if c.FinishReason == "stop" {
					choice0Finished = true
				}
			} else if c.Index == 1 {
				if c.Delta.Content != nil {
					choice1HasContent = true
				}
				if len(c.Delta.ToolCalls) > 0 {
					choice1HasToolCalls = true
				}
				if c.FinishReason == "tool_calls" {
					choice1Finished = true
				}
			}
		}
	}

	if !choice0Finished {
		t.Fatal("expected Choice 0 to finish with stop")
	}
	if !choice0HasContent {
		t.Fatal("expected Choice 0 to have injected block text")
	}
	if choice0HasToolCalls {
		t.Fatal("expected Choice 0 to have no tool calls in rewrite")
	}
	if !choice1Finished {
		t.Fatal("expected Choice 1 to finish with tool_calls")
	}
	if !choice1HasToolCalls {
		t.Fatal("expected Choice 1 to retain its tool calls")
	}
	if choice1HasContent {
		t.Fatal("expected Choice 1 to have no injected block text")
	}
}

func TestOpenAIResponseRewriterPreservesInterleavingChatSSE(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := []byte(strings.Join([]string{
		`data: {"id":"chatcmpl_interleave","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"leading0"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_interleave","object":"chat.completion.chunk","choices":[{"index":1,"delta":{"content":"leading1"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_interleave","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"rm\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_interleave","object":"chat.completion.chunk","choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"WebFetch","arguments":"{\"url\":\"https://safe.test\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_interleave","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"id":"chatcmpl_interleave","object":"chat.completion.chunk","choices":[{"index":1,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"id":"chatcmpl_interleave","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"trailing0"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_interleave","object":"chat.completion.chunk","choices":[{"index":1,"delta":{"content":"trailing1"},"finish_reason":null}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))

	// Block Bash on Choice 0, allow WebFetch on Choice 1
	result, err := rewriter.Rewrite(body, "text/event-stream", func(tu ToolUse) ToolUseVerdict {
		if tu.Name == "Bash" {
			return ToolUseVerdict{Allowed: false, Reason: "requires approval"}
		}
		return ToolUseVerdict{Allowed: true}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected rewritten response")
	}

	events := parseTestSSEEvents(t, string(result.Body))
	var sequence []string

	for _, ev := range events {
		var payload struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content   any   `json:"content"`
					ToolCalls []any `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			continue
		}
		for _, c := range payload.Choices {
			if txt := flattenOpenAIContentFromAny(c.Delta.Content); txt != "" {
				sequence = append(sequence, fmt.Sprintf("content_%d_%s", c.Index, txt))
			}
			if len(c.Delta.ToolCalls) > 0 {
				sequence = append(sequence, fmt.Sprintf("tool_calls_%d", c.Index))
			}
			if c.FinishReason == "tool_calls" {
				sequence = append(sequence, fmt.Sprintf("finish_tool_calls_%d", c.Index))
			}
			if c.FinishReason == "stop" {
				sequence = append(sequence, fmt.Sprintf("finish_stop_%d", c.Index))
			}
		}
	}

	// The expected sequence preserves exact relative order of events:
	expected := []string{
		"content_0_leading0",
		"content_1_leading1",
		"tool_calls_1",
		"content_0_Tool 'Bash' was blocked by Clawvisor policy: requires approval",
		"finish_stop_0",
		"finish_tool_calls_1",
		"content_0_trailing0",
		"content_1_trailing1",
	}

	if len(sequence) != len(expected) {
		t.Fatalf("expected sequence length %d, got %d. Sequence:\n%v", len(expected), len(sequence), sequence)
	}
	for i, val := range expected {
		if sequence[i] != val {
			t.Fatalf("at index %d: expected %q, got %q", i, val, sequence[i])
		}
	}

	if result.AssistantTurn == nil {
		t.Fatal("expected AssistantTurn to be populated")
	}
	expectedTurnContent := strings.Join([]string{
		"leading0",
		"Tool 'Bash' was blocked by Clawvisor policy: requires approval",
		"trailing0",
		"leading1",
		`<tool_use name=WebFetch input={"url":"https://safe.test"}>`,
		"trailing1",
	}, "\n")
	if result.AssistantTurn.Content != expectedTurnContent {
		t.Errorf("expected AssistantTurn content:\n%q\n\ngot:\n%q", expectedTurnContent, result.AssistantTurn.Content)
	}
}

func TestOpenAIResponseRewriterMixedToolCallsChatSSE(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := []byte(strings.Join([]string{
		`data: {"id":"chatcmpl_mixed","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_mixed","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"rm\"}"}},{"index":1,"id":"call_2","type":"function","function":{"name":"WebFetch","arguments":"{\"url\":\"https://safe.test\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_mixed","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))

	// Block Bash, allow WebFetch
	result, err := rewriter.Rewrite(body, "text/event-stream", func(tu ToolUse) ToolUseVerdict {
		if tu.Name == "Bash" {
			return ToolUseVerdict{Allowed: false, Reason: "requires approval"}
		}
		return ToolUseVerdict{Allowed: true}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected rewritten response")
	}

	events := parseTestSSEEvents(t, string(result.Body))
	var choice0Finished bool
	var choice0HasContent, choice0HasToolCalls bool

	for _, ev := range events {
		var payload struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content   any   `json:"content"`
					ToolCalls []any `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			continue
		}
		for _, c := range payload.Choices {
			if c.Index == 0 {
				if c.Delta.Content != nil {
					choice0HasContent = true
					txt := flattenOpenAIContentFromAny(c.Delta.Content)
					if !strings.Contains(txt, "requires approval") {
						t.Fatalf("expected Choice 0 content to contain block message, got %q", txt)
					}
				}
				if len(c.Delta.ToolCalls) > 0 {
					choice0HasToolCalls = true
					// Check that only WebFetch is present, Bash is stripped
					tcMap := c.Delta.ToolCalls[0].(map[string]any)
					fn := tcMap["function"].(map[string]any)
					name := fn["name"].(string)
					if name != "WebFetch" {
						t.Fatalf("expected only WebFetch allowed tool call, got %q", name)
					}
				}
				if c.FinishReason == "tool_calls" {
					choice0Finished = true
				}
			}
		}
	}

	if !choice0Finished {
		t.Fatal("expected Choice 0 to finish with tool_calls")
	}
	if !choice0HasContent {
		t.Fatal("expected Choice 0 to have injected block text")
	}
	if !choice0HasToolCalls {
		t.Fatal("expected Choice 0 to have allowed tool call")
	}
}

func TestOpenAIResponseRewriterIncrementalArgumentsChatSSE(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("expected OpenAI response rewriter")
	}

	body := []byte(strings.Join([]string{
		`data: {"id":"chatcmpl_inc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_inc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"WebFetch","arguments":""}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_inc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"url\":"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_inc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"https://unsafe.test\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_inc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))

	// Rewrite input from unsafe.test to safe.test
	result, err := rewriter.Rewrite(body, "text/event-stream", func(tu ToolUse) ToolUseVerdict {
		if tu.Name == "WebFetch" {
			return ToolUseVerdict{
				Allowed:      true,
				RewriteInput: []byte(`{"url":"https://safe.test"}`),
			}
		}
		return ToolUseVerdict{Allowed: true}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected rewritten response")
	}

	events := parseTestSSEEvents(t, string(result.Body))
	var argumentDeltas []string

	for _, ev := range events {
		var payload struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			continue
		}
		for _, c := range payload.Choices {
			for _, tc := range c.Delta.ToolCalls {
				if tc.Function.Arguments != "" {
					argumentDeltas = append(argumentDeltas, tc.Function.Arguments)
				}
			}
		}
	}

	// The original arguments had deltas of length: 7 (`{"url":`) and 21 (`"https://unsafe.test"}`).
	// Total length = 28.
	// The rewritten arguments have length 26 (`{"url":"https://safe.test"}`).
	// Proportion for chunk 1: 7/28 = 0.25. 26 * 0.25 = 6.5 -> 6 bytes.
	// Chunk 1 rewritten arguments should be 6 bytes: `{"url"`
	// Chunk 2 rewritten arguments should be the rest: `:"https://safe.test"}`
	if len(argumentDeltas) != 2 {
		t.Fatalf("expected 2 argument deltas, got %v", argumentDeltas)
	}

	joined := strings.Join(argumentDeltas, "")
	if joined != `{"url":"https://safe.test"}` {
		t.Errorf("expected joined arguments %q, got %q", `{"url":"https://safe.test"}`, joined)
	}

	// Verify individual deltas are proportional
	if argumentDeltas[0] != `{"url"` {
		t.Errorf("expected delta 0 to be %q, got %q", `{"url"`, argumentDeltas[0])
	}
	if argumentDeltas[1] != `:"https://safe.test"}` {
		t.Errorf("expected delta 1 to be %q, got %q", `:"https://safe.test"}`, argumentDeltas[1])
	}
}
