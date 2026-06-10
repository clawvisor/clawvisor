package conversation

import "testing"

func TestAnthropicApprovalReplyBareReplyUsesAssistantMarker(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "do the thing"},
			{"role": "assistant", "content": "Clawvisor paused this tool call for approval.\n\n[clawvisor:approval=cv-abcdefghij12]"},
			{"role": "user", "content": "y"}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "approve" {
		t.Fatalf("verb = %q, want approve", verb)
	}
	if id != "cv-abcdefghij12" {
		t.Fatalf("id = %q, want marker from assistant transcript", id)
	}
}

func TestAnthropicApprovalReplyExplicitIDWinsOverMarker(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": "[clawvisor:approval=cv-bbbbbbbbbbbb]"},
			{"role": "user", "content": "approve cv-aaaaaaaaaaaa"}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "approve" || id != "cv-aaaaaaaaaaaa" {
		t.Fatalf("explicit ID should win; got verb=%q id=%q", verb, id)
	}
}

func TestAnthropicApprovalReplyPicksMostRecentAssistantMarker(t *testing.T) {
	// Two pending approvals in the same transcript: the user is replying
	// to the most recent prompt, so the most recent marker should win.
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": "first prompt [clawvisor:approval=cv-aaaaaaaaaaaa]"},
			{"role": "user", "content": "looking at the next one"},
			{"role": "assistant", "content": "second prompt [clawvisor:approval=cv-bbbbbbbbbbbb]"},
			{"role": "user", "content": "y"}
		]
	}`)
	_, id := AnthropicApprovalReply(body)
	if id != "cv-bbbbbbbbbbbb" {
		t.Fatalf("id = %q, want most recent marker cv-bbbbbbbbbbbb", id)
	}
}

func TestAnthropicApprovalReplyNoMarkerLeavesIDEmpty(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": "Clawvisor paused this tool call for approval."},
			{"role": "user", "content": "y"}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "approve" || id != "" {
		t.Fatalf("verb=%q id=%q, want approve and empty id (LIFO fallback)", verb, id)
	}
}

func TestAnthropicApprovalReplyBlockContentMarker(t *testing.T) {
	// Assistant content in Anthropic format is often an array of typed blocks.
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [{"type": "text", "text": "paused.\n\n[clawvisor:approval=cv-abcdefghij34]"}]},
			{"role": "user", "content": [{"type": "text", "text": "y"}]}
		]
	}`)
	_, id := AnthropicApprovalReply(body)
	if id != "cv-abcdefghij34" {
		t.Fatalf("id = %q, want cv-abcdefghij34 from typed block content", id)
	}
}

func TestOpenAIApprovalReplyChatMessagesBareReplyUsesMarker(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": "paused [clawvisor:approval=cv-cccccccccccc]"},
			{"role": "user", "content": "y"}
		]
	}`)
	verb, id := OpenAIApprovalReply(body)
	if verb != "approve" || id != "cv-cccccccccccc" {
		t.Fatalf("verb=%q id=%q, want approve cv-cccccccccccc", verb, id)
	}
}

func TestOpenAIApprovalReplyResponsesInputBareReplyUsesMarker(t *testing.T) {
	// OpenAI Responses API uses a top-level "input" array of typed items
	// instead of the "messages" array.
	body := []byte(`{
		"input": [
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "[clawvisor:approval=cv-dddddddddddd]"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "y"}]}
		]
	}`)
	verb, id := OpenAIApprovalReply(body)
	if verb != "approve" || id != "cv-dddddddddddd" {
		t.Fatalf("verb=%q id=%q, want approve cv-dddddddddddd", verb, id)
	}
}

func TestOpenAIApprovalReplyResponsesInputStaleAfterFunctionCallOutput(t *testing.T) {
	// Regression: after the user's "y" is consumed and the released tool
	// has run, the function_call_output item appears later in the input
	// array than the "y". Subsequent inference requests still carry the
	// "y" in history but it must NOT re-fire the release path, or the
	// proxy will deny every follow-up with "approval no longer valid".
	body := []byte(`{
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "create some files"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "paused [clawvisor:approval=cv-eeeeeeeeeeee]"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "y"}]},
			{"type": "function_call", "name": "exec_command", "call_id": "call_1", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "ok"}
		]
	}`)
	verb, id := OpenAIApprovalReply(body)
	if verb != "" || id != "" {
		t.Fatalf("verb=%q id=%q, want empty (release already happened)", verb, id)
	}
}

func TestOpenAIApprovalReplyResponsesInputStaleAfterPendingFunctionCall(t *testing.T) {
	// Defense: even if only a function_call (no output yet) has appeared
	// after the "y", the model has already moved past the approval turn
	// — the held call was released and the harness is about to execute
	// it. Treat the "y" as stale rather than re-firing the release path.
	body := []byte(`{
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "create some files"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "paused [clawvisor:approval=cv-ffffffffffff]"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "y"}]},
			{"type": "function_call", "name": "exec_command", "call_id": "call_2", "arguments": "{}"}
		]
	}`)
	verb, id := OpenAIApprovalReply(body)
	if verb != "" || id != "" {
		t.Fatalf("verb=%q id=%q, want empty (release already happened)", verb, id)
	}
}

func TestOpenAIApprovalReplyResponsesInputStaleAfterCustomToolCall(t *testing.T) {
	// custom_tool_call is a first-class input item type that Responses
	// round-trips (see sanitizeResponsesItemForInput). A released custom
	// tool call sitting after the user's "y" is the same staleness
	// signal as function_call — the approval was already processed.
	body := []byte(`{
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "apply the patch"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "paused [clawvisor:approval=cv-hhhhhhhhhhhh]"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "y"}]},
			{"type": "custom_tool_call", "name": "apply_patch", "call_id": "ctc_1", "input": "*** Begin Patch\n*** End Patch\n"}
		]
	}`)
	verb, id := OpenAIApprovalReply(body)
	if verb != "" || id != "" {
		t.Fatalf("verb=%q id=%q, want empty (release already happened)", verb, id)
	}
}

func TestOpenAIApprovalReplyResponsesInputStaleAfterToolSearch(t *testing.T) {
	// Regression: Codex's built-in tool-discovery emits tool_search_call
	// and tool_search_output items, which Codex round-trips on
	// continuation requests. A tool_search round-trip sitting after the
	// user's "y" — together with the assistant commentary and reasoning
	// items the model emitted post-approval — means the conversation has
	// moved past the approval turn. Before the fix the proxy missed
	// these item types and re-fired the release path on every
	// follow-up, denying with "approval no longer valid" once the hold
	// had already been consumed.
	body := []byte(`{
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "search happenstance"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "paused [clawvisor:approval=cv-iiiiiiiiiiii]"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "y"}]},
			{"type": "reasoning", "summary": []},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "I'll look for the available tools first."}]},
			{"type": "tool_search_call", "call_id": "call_ts_1", "status": "completed", "arguments": {"query": "happenstance"}},
			{"type": "tool_search_output", "call_id": "call_ts_1", "status": "completed", "tools": []}
		]
	}`)
	verb, id := OpenAIApprovalReply(body)
	if verb != "" || id != "" {
		t.Fatalf("verb=%q id=%q, want empty (conversation moved past approval turn)", verb, id)
	}
}

func TestOpenAIApprovalReplyResponsesInputStaleAfterAssistantMessage(t *testing.T) {
	// Even without any tool activity, an assistant message appearing
	// after the user's "y" means the model has already responded —
	// the release happened on a prior request and the bare "y" lingering
	// in history is stale.
	body := []byte(`{
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "do the thing"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "paused [clawvisor:approval=cv-jjjjjjjjjjjj]"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "y"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "OK, working on it."}]}
		]
	}`)
	verb, id := OpenAIApprovalReply(body)
	if verb != "" || id != "" {
		t.Fatalf("verb=%q id=%q, want empty (assistant already spoke past approval)", verb, id)
	}
}

func TestOpenAIApprovalReplyResponsesInputHeldCallBeforeReplyStillFires(t *testing.T) {
	// Inverse of the stale case: when the held function_call sits BEFORE
	// the "y" (the model emitted it on the prior turn, it was held, then
	// the user replied), the release path must still fire. Only function
	// calls appearing AFTER the user reply indicate the conversation
	// moved past the approval.
	body := []byte(`{
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "create some files"}]},
			{"type": "function_call", "name": "exec_command", "call_id": "call_3", "arguments": "{}"},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "paused [clawvisor:approval=cv-gggggggggggg]"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "y"}]}
		]
	}`)
	verb, id := OpenAIApprovalReply(body)
	if verb != "approve" || id != "cv-gggggggggggg" {
		t.Fatalf("verb=%q id=%q, want approve cv-gggggggggggg", verb, id)
	}
}

// AskUserQuestion-shape coverage lives in
// internal/runtime/llmproxy/askuserquestion_reply_test.go now that the
// AskUserQuestion-aware parser moved out of this package — the
// conversation package only knows about Clawvisor's marker
// conventions and Anthropic wire shapes, not specific harness tool
// names.
