package conversation

import (
	"encoding/json"
	"strings"
	"testing"
)

// The OpenAI Responses API documents `custom_tool_call.input` as a
// string. When the synthesized rewrite path re-emits a custom_tool_call
// it must preserve the string shape (escaped JSON string), not promote
// the value to a structured JSON object even when our internal
// ToolUse normalization wrapped the freeform input.
func TestCustomToolCallReemit_PreservesStringInput(t *testing.T) {
	const freeform = "echo hello"
	item := orderedResponsesItem{
		isCustomToolCall: true,
		outputIndex:      0,
		itemID:           "ctc_1",
		callID:           "call_1",
		name:             "shell",
		customInput:      customToolInputForReemit(freeform, nil),
	}
	sse := buildOpenAIResponsesMultiSSE([]orderedResponsesItem{item})
	// Look for the input field shape on a `response.output_item.added`
	// event. The value must serialize as a JSON string (quoted), not
	// as an object.
	got := string(sse)
	if !strings.Contains(got, `"input":"echo hello"`) {
		t.Fatalf("expected string-shaped input field, got:\n%s", got)
	}
	if strings.Contains(got, `"input":{`) {
		t.Errorf("emitted input as an object, but spec types it as a string:\n%s", got)
	}
}

func TestCustomToolInputForReemit_NilCollapsesToNull(t *testing.T) {
	got := customToolInputForReemit(nil, nil)
	if got != nil {
		t.Errorf("nil input should round-trip as nil, got %v", got)
	}
	// Through json.Marshal, a nil any inside a map becomes JSON null.
	out, _ := json.Marshal(map[string]any{"input": got})
	if string(out) != `{"input":null}` {
		t.Errorf("nil should marshal as null, got %s", out)
	}
}

// Regression: when the model sends the payload in `arguments` (some
// SDK clients use that legacy field for custom tools) rather than
// `input`, the re-emitted SSE must preserve the value instead of
// dropping it to null.
func TestCustomToolInputForReemit_FallsBackToArguments(t *testing.T) {
	got := customToolInputForReemit("", "fallback-payload")
	if got != "fallback-payload" {
		t.Errorf("expected fallback to arguments, got %v", got)
	}
	// And whitespace-only `input` should also fall through.
	got = customToolInputForReemit("   ", "real-payload")
	if got != "real-payload" {
		t.Errorf("whitespace-only input should fall back, got %v", got)
	}
}
