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
