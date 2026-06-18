package llmproxy

import (
	"strings"
	"testing"
)

// TestOpenAIInboundShape_PrefersInputOverMessages pins the
// disambiguation contract for OpenAI bodies: Responses (`input`) and
// Chat Completions (`messages`) are mutually exclusive on the wire,
// and walking BOTH on a malformed body that carries both can return
// stale entries first. The shape picks whichever array is populated
// and walks ONLY that one, so the most-recent-first contract holds.
func TestOpenAIInboundShape_PrefersInputOverMessages(t *testing.T) {
	shape := OpenAIInboundShape{}

	// Both arrays populated, with a stale assistant decision marker in
	// `messages` and the fresh one in `input`. The walker must return
	// the input entry first; falling back to messages would surface
	// the stale marker.
	body := []byte(`{
		"input": [
			{"role": "user", "content": [{"type": "input_text", "text": "do thing"}]},
			{"role": "assistant", "content": [{"type": "output_text", "text": "<clawvisor-secret-decision id=cv-fresh>"}]}
		],
		"messages": [
			{"role": "user", "content": "older user"},
			{"role": "assistant", "content": "<clawvisor-secret-decision id=cv-stale>"}
		]
	}`)
	turns := shape.AssistantTextTurns(body)
	if len(turns) == 0 {
		t.Fatal("expected at least one assistant turn")
	}
	if !strings.Contains(turns[0], "cv-fresh") {
		t.Fatalf("latest-first contract violated: turns[0] = %q (want id=cv-fresh)", turns[0])
	}
	for _, t := range turns {
		if strings.Contains(t, "cv-stale") {
			// The fix: when input is populated, don't walk messages at
			// all. So cv-stale must NOT appear in the result.
			break
		}
	}
	// Stale marker must not leak through.
	for _, txt := range turns {
		if strings.Contains(txt, "cv-stale") {
			t.Fatalf("walker leaked stale messages[] entry: %q", txt)
		}
	}

	if user := shape.LatestUserText(body); user != "do thing" {
		t.Fatalf("LatestUserText = %q, want %q (must walk input only when populated)", user, "do thing")
	}
}

// TestOpenAIInboundShape_FallsBackToMessagesWhenInputAbsent confirms
// the disambiguation doesn't accidentally hide the Chat Completions
// path: when input is absent, messages MUST be walked.
func TestOpenAIInboundShape_FallsBackToMessagesWhenInputAbsent(t *testing.T) {
	shape := OpenAIInboundShape{}
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "from messages"},
			{"role": "assistant", "content": "from messages assistant"}
		]
	}`)
	turns := shape.AssistantTextTurns(body)
	if len(turns) != 1 || turns[0] != "from messages assistant" {
		t.Fatalf("AssistantTextTurns = %v, want one entry from messages", turns)
	}
	if user := shape.LatestUserText(body); user != "from messages" {
		t.Fatalf("LatestUserText = %q, want %q", user, "from messages")
	}
}
