package llmproxy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestBuildContinuationBody_RejectsEmptyToolResults(t *testing.T) {
	_, err := BuildContinuationBody(conversation.ProviderAnthropic, "application/json",
		[]byte(`{"messages":[]}`), []byte(`{}`), nil)
	if err == nil {
		t.Fatalf("expected error on empty tool_results, got nil")
	}
}

func TestBuildContinuationBody_OpenAIUnknownShape(t *testing.T) {
	// Body with neither messages nor input — we can't tell which
	// OpenAI surface to target, so the builder must error rather
	// than guess.
	_, err := BuildContinuationBody(conversation.ProviderOpenAI, "application/json",
		[]byte(`{"model":"gpt-4"}`), []byte(`{}`),
		[]ContinuationToolResult{{ToolUseID: "call_1", Content: "ok"}})
	if err == nil {
		t.Fatal("expected error on indeterminate openai shape")
	}
	if !strings.Contains(err.Error(), "openai request shape") {
		t.Errorf("error did not mention shape: %v", err)
	}
}

func TestBuildOpenAIChatContinuationBody_JSON(t *testing.T) {
	originalBody := []byte(`{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "make files"}],
		"tools": [{"type": "function", "function": {"name": "Bash"}}]
	}`)
	upstreamResponse := []byte(`{
		"id": "chatcmpl-x",
		"object": "chat.completion",
		"model": "gpt-4",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{
					"id": "call_abc",
					"type": "function",
					"function": {"name": "Bash", "arguments": "{\"cmd\":\"curl https://clawvisor.local/control/tasks\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)
	out, err := BuildContinuationBody(
		conversation.ProviderOpenAI,
		"application/json",
		originalBody,
		upstreamResponse,
		[]ContinuationToolResult{{ToolUseID: "call_abc", Content: "[Clawvisor: task approved]"}},
	)
	if err != nil {
		t.Fatalf("BuildContinuationBody: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	if parsed["model"] != "gpt-4" {
		t.Errorf("model not preserved: %v", parsed["model"])
	}
	if _, ok := parsed["tools"].([]any); !ok {
		t.Errorf("tools field not preserved")
	}
	msgs, ok := parsed["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("expected 3 messages (user, assistant, tool); got %v", msgs)
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Errorf("msg[1] role: %v", asst["role"])
	}
	calls, ok := asst["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant tool_calls missing or wrong shape: %v", asst["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_abc" {
		t.Errorf("tool_call id: %v", call["id"])
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "Bash" {
		t.Errorf("function name: %v", fn["name"])
	}
	if !strings.Contains(fn["arguments"].(string), "curl") {
		t.Errorf("function arguments lost: %v", fn["arguments"])
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" {
		t.Errorf("msg[2] role: %v", tool["role"])
	}
	if tool["tool_call_id"] != "call_abc" {
		t.Errorf("tool_call_id mismatch: %v", tool["tool_call_id"])
	}
	if !strings.Contains(tool["content"].(string), "task approved") {
		t.Errorf("tool content lost: %v", tool["content"])
	}
}

func TestBuildOpenAIChatContinuationBody_SSE(t *testing.T) {
	originalBody := []byte(`{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "make files"}],
		"stream": true
	}`)
	// Minimal Chat Completions SSE: tool_call assembled from two deltas.
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-y","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_sse","type":"function","function":{"name":"Bash","arguments":""}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-y","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"echo hi\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-y","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out, err := BuildContinuationBody(
		conversation.ProviderOpenAI,
		"text/event-stream",
		originalBody,
		[]byte(sse),
		[]ContinuationToolResult{{ToolUseID: "call_sse", Content: "[done]"}},
	)
	if err != nil {
		t.Fatalf("BuildContinuationBody: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	msgs := parsed["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	asst := msgs[1].(map[string]any)
	calls := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool_call reconstructed from SSE, got %d", len(calls))
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_sse" {
		t.Errorf("tool_call id: %v", call["id"])
	}
	args := call["function"].(map[string]any)["arguments"].(string)
	if !strings.Contains(args, "echo hi") {
		t.Errorf("arguments not reassembled from SSE deltas: %v", args)
	}
}

func TestBuildOpenAIResponsesContinuationBody_StringInput(t *testing.T) {
	originalBody := []byte(`{
		"model": "gpt-4",
		"input": "make me a file"
	}`)
	upstreamResponse := []byte(`{
		"id": "resp_x",
		"object": "response",
		"output": [
			{"type": "function_call", "id": "fc_1", "call_id": "call_resp", "name": "Bash", "arguments": "{\"cmd\":\"curl\"}", "status": "completed"}
		]
	}`)
	out, err := BuildContinuationBody(
		conversation.ProviderOpenAI,
		"application/json",
		originalBody,
		upstreamResponse,
		[]ContinuationToolResult{{ToolUseID: "call_resp", Content: "[ok]"}},
	)
	if err != nil {
		t.Fatalf("BuildContinuationBody: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	input, ok := parsed["input"].([]any)
	if !ok {
		t.Fatalf("input should have been promoted to array, got %T", parsed["input"])
	}
	if len(input) != 3 {
		t.Fatalf("expected 3 input items (promoted-user + function_call + function_call_output); got %d: %v", len(input), input)
	}
	// First item: the promoted string-as-user-message.
	first := input[0].(map[string]any)
	if first["type"] != "message" || first["role"] != "user" {
		t.Errorf("first item should be promoted user message: %v", first)
	}
	// Second item: the function_call from the upstream output.
	second := input[1].(map[string]any)
	if second["type"] != "function_call" || second["call_id"] != "call_resp" {
		t.Errorf("second item should be the function_call: %v", second)
	}
	if _, hasStatus := second["status"]; hasStatus {
		t.Errorf("function_call should have had status stripped: %v", second)
	}
	// Third item: function_call_output keyed off the same call_id.
	third := input[2].(map[string]any)
	if third["type"] != "function_call_output" {
		t.Errorf("third item should be function_call_output: %v", third)
	}
	if third["call_id"] != "call_resp" {
		t.Errorf("call_id mismatch on function_call_output: %v", third)
	}
	if !strings.Contains(third["output"].(string), "[ok]") {
		t.Errorf("function_call_output content lost: %v", third["output"])
	}
}

func TestBuildOpenAIResponsesContinuationBody_ArrayInput(t *testing.T) {
	originalBody := []byte(`{
		"model": "gpt-4",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "first"}]}
		]
	}`)
	upstreamResponse := []byte(`{
		"output": [
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "thinking"}], "status": "completed"},
			{"type": "function_call", "id": "fc_1", "call_id": "call_x", "name": "Bash", "arguments": "{}", "status": "completed"}
		]
	}`)
	out, err := BuildContinuationBody(
		conversation.ProviderOpenAI,
		"application/json",
		originalBody,
		upstreamResponse,
		[]ContinuationToolResult{{ToolUseID: "call_x", Content: "[r]"}},
	)
	if err != nil {
		t.Fatalf("BuildContinuationBody: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	input := parsed["input"].([]any)
	// Original 1 item + 2 output items (assistant message + function_call) + 1 function_call_output = 4
	if len(input) != 4 {
		t.Fatalf("expected 4 input items, got %d: %v", len(input), input)
	}
	// Verify the original is still at position 0.
	if input[0].(map[string]any)["type"] != "message" || input[0].(map[string]any)["role"] != "user" {
		t.Errorf("original user message moved; got input[0]=%v", input[0])
	}
	// Last item is function_call_output.
	last := input[len(input)-1].(map[string]any)
	if last["type"] != "function_call_output" {
		t.Errorf("last item should be function_call_output, got %v", last["type"])
	}
}

func TestBuildOpenAIResponsesContinuationBody_SSE(t *testing.T) {
	originalBody := []byte(`{"model": "gpt-4", "input": "x"}`)
	// SSE shape: response.output_item.done events carry the final
	// items including assembled arguments.
	sse := strings.Join([]string{
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_sse_r","name":"Bash","arguments":"{\"cmd\":\"echo\"}","status":"completed"}}`,
		``,
	}, "\n")
	out, err := BuildContinuationBody(
		conversation.ProviderOpenAI,
		"text/event-stream",
		originalBody,
		[]byte(sse),
		[]ContinuationToolResult{{ToolUseID: "call_sse_r", Content: "[ok]"}},
	)
	if err != nil {
		t.Fatalf("BuildContinuationBody: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	input := parsed["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("expected 3 input items, got %d", len(input))
	}
	if input[1].(map[string]any)["call_id"] != "call_sse_r" {
		t.Errorf("function_call call_id from SSE: %v", input[1])
	}
}

// TestBuildOpenAIResponsesContinuationBody_RefusesBuiltInToolItems
// asserts the continuation builder errors out cleanly when the
// upstream's output contains an item type the Responses API rejects
// on input[]. The handler treats the returned error as a soft
// failure and falls back to SubstituteWith, which is safer than
// issuing a continuation forward we know will 400.
func TestBuildOpenAIResponsesContinuationBody_RefusesBuiltInToolItems(t *testing.T) {
	originalBody := []byte(`{"model":"gpt-4","input":"x"}`)
	upstream := []byte(`{
		"output": [
			{"type": "web_search_call", "id": "ws_1", "status": "completed"},
			{"type": "function_call", "id": "fc_1", "call_id": "call_x", "name": "Bash", "arguments": "{}", "status": "completed"}
		]
	}`)
	_, err := BuildContinuationBody(
		conversation.ProviderOpenAI,
		"application/json",
		originalBody,
		upstream,
		[]ContinuationToolResult{{ToolUseID: "call_x", Content: "[ok]"}},
	)
	if err == nil {
		t.Fatal("expected continuation to refuse when output contains a built-in tool call item; got nil error")
	}
	if !errors.Is(err, conversation.ErrResponsesContinuationHasBuiltInToolItem) {
		t.Errorf("expected ErrResponsesContinuationHasBuiltInToolItem wrapped; got %v", err)
	}
}

// TestBuildOpenAIResponsesContinuationBody_AcceptsReasoning confirms
// reasoning items still round-trip — they're input-acceptable on the
// Responses API and the model needs them back for chain-of-thought
// continuity across our synthetic function_call_output.
func TestBuildOpenAIResponsesContinuationBody_AcceptsReasoning(t *testing.T) {
	originalBody := []byte(`{"model":"gpt-4","input":"x"}`)
	upstream := []byte(`{
		"output": [
			{"type": "reasoning", "id": "rs_1", "summary": [{"type":"summary_text","text":"thinking"}], "status": "completed"},
			{"type": "function_call", "id": "fc_1", "call_id": "call_x", "name": "Bash", "arguments": "{}", "status": "completed"}
		]
	}`)
	out, err := BuildContinuationBody(
		conversation.ProviderOpenAI,
		"application/json",
		originalBody,
		upstream,
		[]ContinuationToolResult{{ToolUseID: "call_x", Content: "[ok]"}},
	)
	if err != nil {
		t.Fatalf("BuildContinuationBody: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	input := parsed["input"].([]any)
	// Promoted user message + reasoning + function_call + function_call_output = 4
	if len(input) != 4 {
		t.Fatalf("expected 4 input items (got %d): %v", len(input), input)
	}
	types := map[string]bool{}
	for _, it := range input {
		if m, ok := it.(map[string]any); ok {
			if t, ok := m["type"].(string); ok {
				types[t] = true
			}
		}
	}
	for _, want := range []string{"reasoning", "function_call", "function_call_output"} {
		if !types[want] {
			t.Errorf("input missing built-in type %q after sanitize: %v", want, input)
		}
	}
}

func TestBuildAnthropicContinuationBody_JSON(t *testing.T) {
	originalBody := []byte(`{
		"model": "claude-sonnet-4",
		"system": "you are helpful",
		"messages": [
			{"role": "user", "content": "create some files"}
		],
		"max_tokens": 1024,
		"stream": false
	}`)
	upstreamResponse := []byte(`{
		"id": "msg_abc",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-4",
		"content": [
			{"type": "text", "text": "I'll create those files."},
			{"type": "tool_use", "id": "toolu_xyz", "name": "Bash", "input": {"cmd": "curl https://clawvisor.local/control/tasks ..."}}
		],
		"stop_reason": "tool_use"
	}`)
	out, err := BuildContinuationBody(
		conversation.ProviderAnthropic,
		"application/json",
		originalBody,
		upstreamResponse,
		[]ContinuationToolResult{{ToolUseID: "toolu_xyz", Content: "[Clawvisor: task was approved. proceed.]"}},
	)
	if err != nil {
		t.Fatalf("BuildContinuationBody: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	// Top-level fields preserved.
	if parsed["model"] != "claude-sonnet-4" {
		t.Errorf("model not preserved: %v", parsed["model"])
	}
	if parsed["system"] != "you are helpful" {
		t.Errorf("system not preserved: %v", parsed["system"])
	}
	if parsed["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens not preserved: %v", parsed["max_tokens"])
	}
	// Messages were extended by two turns.
	msgs, ok := parsed["messages"].([]any)
	if !ok {
		t.Fatalf("messages not an array: %T", parsed["messages"])
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (user+assistant+user), got %d: %v", len(msgs), msgs)
	}
	// New assistant turn contains the upstream tool_use, verbatim.
	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Errorf("turn 2 should be assistant, got role=%v", assistant["role"])
	}
	aContent := assistant["content"].([]any)
	if len(aContent) != 2 {
		t.Fatalf("expected 2 assistant content blocks, got %d", len(aContent))
	}
	textBlock := aContent[0].(map[string]any)
	if textBlock["type"] != "text" || textBlock["text"] != "I'll create those files." {
		t.Errorf("text block lost: %v", textBlock)
	}
	tuBlock := aContent[1].(map[string]any)
	if tuBlock["type"] != "tool_use" || tuBlock["id"] != "toolu_xyz" || tuBlock["name"] != "Bash" {
		t.Errorf("tool_use block malformed: %v", tuBlock)
	}
	// New user turn carries the tool_result, addressed to the same tool_use_id.
	user := msgs[2].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("turn 3 should be user, got role=%v", user["role"])
	}
	uContent := user["content"].([]any)
	if len(uContent) != 1 {
		t.Fatalf("expected 1 tool_result block, got %d", len(uContent))
	}
	trBlock := uContent[0].(map[string]any)
	if trBlock["type"] != "tool_result" {
		t.Errorf("expected tool_result, got %v", trBlock["type"])
	}
	if trBlock["tool_use_id"] != "toolu_xyz" {
		t.Errorf("tool_use_id mismatch: %v", trBlock["tool_use_id"])
	}
	if !strings.Contains(trBlock["content"].(string), "task was approved") {
		t.Errorf("content lost: %v", trBlock["content"])
	}
}

func TestBuildAnthropicContinuationBody_SSE(t *testing.T) {
	originalBody := []byte(`{
		"model": "claude-sonnet-4",
		"messages": [{"role": "user", "content": "create files"}],
		"stream": true
	}`)
	// Minimal Anthropic SSE: message_start + a tool_use block_start +
	// input_json_delta + content_block_stop.
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_sse","role":"assistant","model":"claude-sonnet-4"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_sse","name":"Bash","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"echo hi\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
	}, "\n")

	out, err := BuildContinuationBody(
		conversation.ProviderAnthropic,
		"text/event-stream",
		originalBody,
		[]byte(sse),
		[]ContinuationToolResult{{ToolUseID: "toolu_sse", Content: "[done]"}},
	)
	if err != nil {
		t.Fatalf("BuildContinuationBody: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	msgs := parsed["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	assistant := msgs[1].(map[string]any)
	aContent := assistant["content"].([]any)
	if len(aContent) != 1 {
		t.Fatalf("expected 1 assistant content block from SSE, got %d", len(aContent))
	}
	tu := aContent[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "toolu_sse" || tu["name"] != "Bash" {
		t.Errorf("tool_use block from SSE malformed: %v", tu)
	}
	input := tu["input"].(map[string]any)
	if input["cmd"] != "echo hi" {
		t.Errorf("tool_use input not reconstructed from SSE deltas: %v", input)
	}
}

func TestBuildAnthropicContinuationBody_RejectsMissingMessages(t *testing.T) {
	_, err := BuildContinuationBody(
		conversation.ProviderAnthropic,
		"application/json",
		[]byte(`{"model":"claude-sonnet-4"}`), // no messages field
		[]byte(`{"content":[{"type":"text","text":"hi"}]}`),
		[]ContinuationToolResult{{ToolUseID: "toolu_x", Content: "ok"}},
	)
	if err == nil {
		t.Fatalf("expected error when original request body has no messages")
	}
	if !strings.Contains(err.Error(), "no messages field") {
		t.Errorf("error did not name the missing field: %v", err)
	}
}

func TestBuildAnthropicContinuationBody_SkipsEmptyToolUseID(t *testing.T) {
	// Continuation requires at least one non-blank tool_use_id; if the
	// caller passes only blank IDs, refuse rather than produce a body
	// the upstream would reject.
	_, err := BuildContinuationBody(
		conversation.ProviderAnthropic,
		"application/json",
		[]byte(`{"messages":[{"role":"user","content":"x"}]}`),
		[]byte(`{"content":[{"type":"text","text":"hi"}]}`),
		[]ContinuationToolResult{{ToolUseID: "   ", Content: "ok"}},
	)
	if err == nil {
		t.Fatalf("expected error when all tool_use_ids are blank")
	}
}

