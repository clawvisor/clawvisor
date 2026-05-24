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

func TestBuildContinuationBody_OpenAIUnsupported(t *testing.T) {
	_, err := BuildContinuationBody(conversation.ProviderOpenAI, "application/json",
		[]byte(`{"messages":[]}`), []byte(`{}`),
		[]ContinuationToolResult{{ToolUseID: "toolu_1", Content: "ok"}})
	if !errors.Is(err, ErrContinuationUnsupportedProvider) {
		t.Fatalf("expected ErrContinuationUnsupportedProvider, got %v", err)
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

func TestContinuationDepthCtx_RoundTrip(t *testing.T) {
	ctx := WithContinuationDepth(nil, 7)
	if got := ContinuationDepthFromContext(ctx); got != 7 {
		t.Errorf("depth round-trip: got %d, want 7", got)
	}
	if got := ContinuationDepthFromContext(nil); got != 0 {
		t.Errorf("nil context: got %d, want 0", got)
	}
}
