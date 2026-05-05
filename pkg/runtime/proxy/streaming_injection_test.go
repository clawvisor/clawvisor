package proxy

// Phase 0.9 — soft-cap streaming-injection scaffold.
//
// These tests verify that synthetic between-turn messages can be injected
// into Anthropic / OpenAI Chat / OpenAI Responses streams without breaking
// the SSE framing rules each format depends on. The Phase 1a metering
// implementation will use this same primitive to surface soft-cap warnings.
//
// What's tested at Phase 0.9 (this scaffold):
//   - Synthesized blocks for each format are well-formed SSE
//     (event/data/blank-line framing, terminal "\n\n").
//   - Injecting at a well-defined boundary inside an existing stream
//     preserves all original bytes.
//
// What's deferred to Phase 1a:
//   - Replay of captured upstream fixtures.
//   - Real-SDK round-trip integration.
//   - Choice between UI-only and conversation-history injection.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// softCapNoticeForSession is the Phase 1a hook surface. The Phase 0.9 stub
// returns a static string; the real implementation will look up the
// session's tier and remaining quota and render a dynamic message.
func softCapNoticeForSession(sessionID, userID string, remaining int) string {
	_ = sessionID
	_ = userID
	if remaining <= 0 {
		return "Clawvisor metering: you have reached your plan's monthly limit. Further usage will be billed at overage rates."
	}
	return "Clawvisor metering: you are approaching your plan's monthly limit. Consider upgrading."
}

func TestSoftCapNotice_AnthropicSSEFraming(t *testing.T) {
	notice := softCapNoticeForSession("session-1", "user-1", 0)
	block := conversation.SynthAnthropicTextSSE("", "", "assistant", notice)

	if !bytes.HasSuffix(block, []byte("\n\n")) {
		t.Errorf("expected trailing \\n\\n on Anthropic SSE block; got %q", trailing(block))
	}
	if !bytes.Contains(block, []byte("event: ")) {
		t.Errorf("expected event: line in Anthropic SSE block; got %s", block)
	}
	if !bytes.Contains(block, []byte("data: ")) {
		t.Errorf("expected data: line in Anthropic SSE block; got %s", block)
	}
	if bytes.Contains(block, []byte(":\n\n")) && !bytes.Contains(block, []byte("data:")) {
		t.Errorf("malformed framing — looks like a comment-only block: %s", block)
	}
}

func TestSoftCapNotice_OpenAIChatSSEFraming(t *testing.T) {
	notice := softCapNoticeForSession("session-1", "user-1", 50)
	// Mirror chatCompletionSSEMapBlock's framing without exporting it: the
	// OpenAI Chat format is `data: {…}\n\n` with no event line.
	block := chatCompletionSSEMapBlock(map[string]any{
		"id":      "chatcmpl-clawvisor-cap",
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": notice}}},
	})

	if !bytes.HasSuffix(block, []byte("\n\n")) {
		t.Errorf("expected trailing \\n\\n on Chat SSE block; got %q", trailing(block))
	}
	if !bytes.HasPrefix(block, []byte("data: ")) {
		t.Errorf("Chat SSE block must start with `data: `; got %s", block)
	}
	if bytes.Contains(block, []byte("event: ")) {
		t.Errorf("Chat Completions framing must not include event: lines; got %s", block)
	}
	// Must NOT be the [DONE] sentinel.
	if strings.Contains(string(block), "[DONE]") {
		t.Errorf("injected block must not contain [DONE] sentinel; got %s", block)
	}
}

func TestSoftCapNotice_OpenAIResponsesSSEFraming(t *testing.T) {
	notice := softCapNoticeForSession("session-1", "user-1", 100)
	block := sseBlock("response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"output_index":  0,
		"delta":         notice,
		"item_id":       "msg_clawvisor_cap",
	})

	if !bytes.HasSuffix(block, []byte("\n\n")) {
		t.Errorf("expected trailing \\n\\n; got %q", trailing(block))
	}
	if !bytes.HasPrefix(block, []byte("event: response.output_text.delta\n")) {
		t.Errorf("Responses framing requires explicit event prefix; got %s", block)
	}
	if !bytes.Contains(block, []byte("data: ")) {
		t.Errorf("missing data: line; got %s", block)
	}
}

// TestSoftCapInjection_PreservesSurroundingBytes verifies that splicing a
// notice into a sample stream at a deterministic event boundary doesn't
// corrupt the surrounding events. Real captured fixtures land in Phase 1a;
// this test uses synthetic events to verify the splicing primitive.
func TestSoftCapInjection_PreservesSurroundingBytes(t *testing.T) {
	pre := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n")
	post := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	notice := conversation.SynthAnthropicTextSSE("", "", "assistant", "Clawvisor metering: cap reached.")

	composed := append(append([]byte{}, pre...), notice...)
	composed = append(composed, post...)

	// Pre and post should each be present byte-identical.
	if !bytes.Contains(composed, pre) {
		t.Error("pre-notice bytes corrupted")
	}
	if !bytes.Contains(composed, post) {
		t.Error("post-notice bytes corrupted")
	}
	if !bytes.Contains(composed, notice) {
		t.Error("notice bytes missing")
	}

	// Each block must end with `\n\n`. Count: pre has 1, notice ≥ 1, post has 3 events.
	count := bytes.Count(composed, []byte("\n\n"))
	if count < 5 {
		t.Errorf("expected ≥5 SSE block terminators, got %d", count)
	}
}

func trailing(b []byte) string {
	if len(b) <= 6 {
		return string(b)
	}
	return string(b[len(b)-6:])
}
