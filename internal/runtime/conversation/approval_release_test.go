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

func TestAnthropicApprovalReplyAskUserQuestionToolResultApproves(t *testing.T) {
	// The model surfaced the inline-approval prompt through AskUserQuestion;
	// the user clicked "yes" and the harness round-tripped the choice as a
	// tool_result. The release path must extract approve + the approval ID
	// from the assistant's question text marker.
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "please do the thing"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_aaa", "name": "AskUserQuestion", "input": {"questions": [{"question": "Clawvisor wants to create a task: do thing.\n\n[clawvisor:approval=cv-askuq00000001]", "options": [{"label": "yes"}, {"label": "no"}]}]}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_aaa", "content": "yes"}
			]}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "approve" {
		t.Fatalf("verb = %q, want approve from AskUserQuestion result", verb)
	}
	if id != "cv-askuq00000001" {
		t.Fatalf("id = %q, want cv-askuq00000001 from question text marker", id)
	}
}

func TestAnthropicApprovalReplyAskUserQuestionToolResultDenies(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_bbb", "name": "AskUserQuestion", "input": {"questions": [{"question": "Approve task X? [clawvisor:approval=cv-askuq00000002]", "options": [{"label": "yes"}, {"label": "no"}]}]}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_bbb", "content": "no"}
			]}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "deny" {
		t.Fatalf("verb = %q, want deny", verb)
	}
	if id != "cv-askuq00000002" {
		t.Fatalf("id = %q, want cv-askuq00000002", id)
	}
}

func TestAnthropicApprovalReplyAskUserQuestionToolResultBlockContent(t *testing.T) {
	// Anthropic's tool_result.content can also be an array of typed blocks
	// (e.g. [{"type":"text","text":"yes"}]). The parser must flatten that
	// shape just like it does for tool_use_result text content elsewhere.
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_ccc", "name": "AskUserQuestion", "input": {"questions": [{"question": "go ahead? [clawvisor:approval=cv-askuq00000003]", "options": [{"label": "yes"}, {"label": "no"}]}]}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_ccc", "content": [{"type": "text", "text": "yes"}]}
			]}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "approve" || id != "cv-askuq00000003" {
		t.Fatalf("verb=%q id=%q, want approve cv-askuq00000003 from block-array tool_result", verb, id)
	}
}

func TestAnthropicApprovalReplyAskUserQuestionWithoutMarkerSkips(t *testing.T) {
	// AskUserQuestion was used but the model did NOT propagate the marker
	// into the question text. We must NOT release a hold without the
	// marker — a bare "yes" without ID would land on whatever the cache
	// picks LIFO, which is the same cross-conversation footgun the marker
	// solves on the text path.
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_ddd", "name": "AskUserQuestion", "input": {"questions": [{"question": "go ahead?", "options": [{"label": "yes"}, {"label": "no"}]}]}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_ddd", "content": "yes"}
			]}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "" || id != "" {
		t.Fatalf("verb=%q id=%q, want empty (no marker → no release)", verb, id)
	}
}

func TestAnthropicApprovalReplyAskUserQuestionIgnoresUnrelatedToolResults(t *testing.T) {
	// A tool_result for a tool_use OTHER than AskUserQuestion must never
	// route through the approval path, even if its content happens to
	// contain "yes". This protects the release path from arbitrary
	// downstream tool outputs that contain the verb token.
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_eee", "name": "Bash", "input": {"command": "echo yes"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_eee", "content": "yes\n"}
			]}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "" || id != "" {
		t.Fatalf("verb=%q id=%q, want empty (non-AskUserQuestion tool_result must not release)", verb, id)
	}
}

func TestAnthropicApprovalReplyAskUserQuestionMarkerInSiblingTextBlock(t *testing.T) {
	// New shape: the inline-approval renderer emits the task body
	// (with the [clawvisor:approval=...] marker) as a sibling text
	// content_block alongside a minimal AskUserQuestion picker —
	// keeps the marker out of the picker question. The parser must
	// scan the assistant turn's text blocks for the marker, not
	// only the AskUserQuestion input.
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "please do the thing"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Clawvisor wants to create a task to cover this work:\n\nPurpose\n  Do thing\n\n[clawvisor:approval=cv-siblingtxt01]"},
				{"type": "tool_use", "id": "toolu_ask_sib", "name": "AskUserQuestion", "input": {"questions": [{"question": "Approve this task?", "options": [{"label": "yes"}, {"label": "no"}]}]}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_ask_sib", "content": "Your questions have been answered: \"Approve this task?\"=\"yes\". You can now continue with these answers in mind."}
			]}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "approve" {
		t.Fatalf("verb = %q, want approve from sibling-text marker", verb)
	}
	if id != "cv-siblingtxt01" {
		t.Fatalf("id = %q, want cv-siblingtxt01 from marker in sibling text block", id)
	}
}

func TestAnthropicApprovalReplyAskUserQuestionClaudeCodeWrapperFormat(t *testing.T) {
	// Claude Code wraps AskUserQuestion answers as
	//   Your questions have been answered: "<question>"="<answer>".
	//   You can now continue with these answers in mind.
	// The verb parser only matches bare yes/no on its own line, so the
	// parser must unwrap the answer first. Real-shape fixture captured
	// from a live test session.
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_clawvisor_ask_cv-claudecode01", "name": "AskUserQuestion", "input": {"questions": [{"question": "Clawvisor wants to create a task to cover this work:\n\nPurpose\n  Do thing\n\n[clawvisor:approval=cv-claudecode01]", "options": [{"label": "yes"}, {"label": "no"}]}]}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_clawvisor_ask_cv-claudecode01", "content": "Your questions have been answered: \"Clawvisor wants to create a task to cover this work:\n\nPurpose\n  Do thing\n\n[clawvisor:approval=cv-claudecode01]\"=\"yes\". You can now continue with these answers in mind."}
			]}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "approve" {
		t.Fatalf("verb = %q, want approve from Claude Code wrapper", verb)
	}
	if id != "cv-claudecode01" {
		t.Fatalf("id = %q, want cv-claudecode01 from marker in question text", id)
	}
}

func TestExtractAskUserQuestionAnswerHandlesUnwrapped(t *testing.T) {
	// Bare-answer harnesses (no wrapper). The extractor returns ""
	// and the caller falls back to running ParseApprovalReplyText
	// on the raw content. Documented here so the fallback contract
	// stays explicit.
	if got := ExtractAskUserQuestionAnswer("yes"); got != "" {
		t.Fatalf("unwrapped answer must NOT match the wrapper RE (caller falls back); got %q", got)
	}
}

func TestExtractAskUserQuestionAnswerHandlesMultipleQuestions(t *testing.T) {
	// Multi-question results have multiple `"="..."` slots. We're
	// only ever using a single question in production, but the
	// extractor's "last match wins" contract should hold so we don't
	// surprise a future caller.
	content := `Your questions have been answered: "q1"="yes","q2"="no". You can now continue with these answers in mind.`
	if got := ExtractAskUserQuestionAnswer(content); got != "no" {
		t.Fatalf("multi-question: want last match \"no\", got %q", got)
	}
}

func TestAnthropicApprovalReplyAskUserQuestionFallbackToTextPath(t *testing.T) {
	// When both shapes are present in the same body (model called
	// AskUserQuestion but also relayed a text "yes"), the text path is
	// the authoritative source — it's processed first in AnthropicApprovalReply.
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_fff", "name": "AskUserQuestion", "input": {"questions": [{"question": "ignored [clawvisor:approval=cv-shouldnotwin]", "options": [{"label": "yes"}, {"label": "no"}]}]}}
			]},
			{"role": "user", "content": "yes"},
			{"role": "assistant", "content": "paused [clawvisor:approval=cv-textpathwins]"},
			{"role": "user", "content": "y"}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "approve" || id != "cv-textpathwins" {
		t.Fatalf("verb=%q id=%q, want text path's cv-textpathwins to win when both shapes coexist", verb, id)
	}
}
