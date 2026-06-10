package conversation

import "testing"

// AskUserQuestion-aware approval-reply parsing lives in this
// package so every caller of the shared AnthropicApprovalReply
// entry point (lite-proxy body editor, runtime proxy, ad-hoc
// tooling) sees the same release behavior. Coverage for the
// AskUserQuestion shape lives here, alongside the helper.

// TestApprovalReplyForProviderSharedEntryPointHandlesAskUserQuestion
// is the regression test for the P1 cubic flagged: the shared entry
// point ApprovalReplyForProvider (which the runtime proxy calls via
// parseApprovalReplyForProvider) MUST recognize AskUserQuestion
// tool_result replies, not just the body editor's overlay. Without
// this, inline approvals release in lite-proxy but silently fail in
// the runtime proxy.
func TestApprovalReplyForProviderSharedEntryPointHandlesAskUserQuestion(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "text", "text": "Clawvisor wants to create a task...\n\n[clawvisor:approval=cv-sharedentry01]"},
				{"type": "tool_use", "id": "toolu_shared", "name": "AskUserQuestion", "input": {"questions": [{"question": "Approve this task?"}]}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_shared", "content": "Your questions have been answered: \"Approve this task?\"=\"yes\". You can now continue with these answers in mind."}
			]}
		]
	}`)
	verb, id := ApprovalReplyForProvider(ProviderAnthropic, body)
	if verb != "approve" || id != "cv-sharedentry01" {
		t.Fatalf("ApprovalReplyForProvider=(%q,%q), want (approve, cv-sharedentry01) — shared entry point must see AskUserQuestion releases", verb, id)
	}
}

func TestAnthropicAskUserQuestionApprovalReplyApproves(t *testing.T) {
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

func TestAnthropicAskUserQuestionApprovalReplyDenies(t *testing.T) {
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

func TestAnthropicAskUserQuestionApprovalReplyBlockContent(t *testing.T) {
	// Anthropic's tool_result.content can also be an array of typed
	// blocks (e.g. [{"type":"text","text":"yes"}]). The parser must
	// flatten that shape just like it does for tool_use_result text
	// content elsewhere.
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

func TestAnthropicAskUserQuestionApprovalReplyWithoutMarkerSkips(t *testing.T) {
	// AskUserQuestion was used but the model did NOT propagate the
	// marker into the question text. We must NOT release a hold
	// without the marker — a bare "yes" without ID would land on
	// whatever the cache picks LIFO, which is the same cross-
	// conversation footgun the marker solves on the text path.
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

func TestAnthropicAskUserQuestionApprovalReplyIgnoresUnrelatedToolResults(t *testing.T) {
	// A tool_result for a tool_use OTHER than AskUserQuestion must
	// never route through the approval path, even if its content
	// happens to contain "yes". This protects the release path from
	// arbitrary downstream tool outputs that contain the verb token.
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

func TestAnthropicAskUserQuestionApprovalReplyMultipleCallsScopeMarkerToTheirOwnText(t *testing.T) {
	// Defense-in-depth: if an assistant turn ever contains TWO
	// AskUserQuestion calls each preceded by its own marker text,
	// the parser must scope each call's marker-search text to the
	// text blocks PRECEDING that call (up to the prior call). A
	// previous broadcast-all implementation matched the FIRST
	// marker for BOTH calls' tool_results, releasing the wrong
	// hold when the user only intended to act on one.
	//
	// In this fixture, the user answers ONLY the second question
	// ("yes"). The parser should release cv-second2 — NOT
	// cv-first1.
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "do two unrelated things"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Clawvisor wants to create a task to cover this work:\n\nPurpose\n  First task\n\n[clawvisor:approval=cv-first1]"},
				{"type": "tool_use", "id": "toolu_call_first", "name": "AskUserQuestion", "input": {"questions": [{"question": "Approve this task?"}]}},
				{"type": "text", "text": "Clawvisor wants to create a task to cover this work:\n\nPurpose\n  Second task\n\n[clawvisor:approval=cv-second2]"},
				{"type": "tool_use", "id": "toolu_call_second", "name": "AskUserQuestion", "input": {"questions": [{"question": "Approve this task?"}]}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_call_second", "content": "Your questions have been answered: \"Approve this task?\"=\"yes\". You can now continue with these answers in mind."}
			]}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "approve" {
		t.Fatalf("verb = %q, want approve", verb)
	}
	if id != "cv-second2" {
		t.Fatalf("id = %q, want cv-second2 (the marker preceding the answered call) — got cv-first1 would mean the first marker bled across calls", id)
	}
}

func TestAnthropicAskUserQuestionApprovalReplyMarkerInSiblingTextBlock(t *testing.T) {
	// The inline-approval renderer emits the task body (with the
	// [clawvisor:approval=...] marker) as a sibling text
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

func TestAnthropicAskUserQuestionApprovalReplyClaudeCodeWrapperFormat(t *testing.T) {
	// Claude Code wraps AskUserQuestion answers as
	//   Your questions have been answered: "<question>"="<answer>".
	//   You can now continue with these answers in mind.
	// The verb parser only matches bare yes/no on its own line, so
	// the parser must unwrap the answer first. Real-shape fixture
	// captured from a live test session.
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
	// extractor's "last match wins" contract should hold so we
	// don't surprise a future caller.
	content := `Your questions have been answered: "q1"="yes","q2"="no". You can now continue with these answers in mind.`
	if got := ExtractAskUserQuestionAnswer(content); got != "no" {
		t.Fatalf("multi-question: want last match \"no\", got %q", got)
	}
}

func TestAnthropicAskUserQuestionApprovalReplyStructuredJSONAnswerPayload(t *testing.T) {
	// AskUserQuestion's input schema documents `answers` as a map
	// from question text to chosen label. A harness that returns
	// the answers as a structured JSON object (instead of the
	// Claude Code text wrapper) must still resolve — the raw-JSON
	// fallback used to return the unparsed bytes, which never
	// matched the verb regex's line-anchored shape.
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_structured", "name": "AskUserQuestion", "input": {"questions": [{"question": "Approve this task? [clawvisor:approval=cv-structured01]"}]}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_structured", "content": {"answers": {"Approve this task?": "yes"}}}
			]}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "approve" {
		t.Fatalf("verb = %q, want approve from structured-JSON answer payload", verb)
	}
	if id != "cv-structured01" {
		t.Fatalf("id = %q, want cv-structured01", id)
	}
}

func TestAnthropicAskUserQuestionApprovalReplyStructuredJSONNestedArrayAnswer(t *testing.T) {
	// Defense-in-depth: a harness emitting answers as an array of
	// strings (rather than a map) should also resolve. Walking
	// leaf strings recursively is what protects us against
	// harness-specific payload shapes.
	body := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_arr", "name": "AskUserQuestion", "input": {"questions": [{"question": "Approve? [clawvisor:approval=cv-arrayshape01]"}]}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_arr", "content": ["yes"]}
			]}
		]
	}`)
	verb, id := AnthropicApprovalReply(body)
	if verb != "approve" {
		t.Fatalf("verb = %q, want approve from array-shaped JSON answer", verb)
	}
	if id != "cv-arrayshape01" {
		t.Fatalf("id = %q, want cv-arrayshape01", id)
	}
}
