package stream_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation/stream"
)

// TestPrependAnthropicAssistantNotice_OutputMatchesLegacy validates the
// Phase 2 proof-of-concept: notice injection via the new event-stream
// model produces output equivalent to the existing
// streaming_assistant_prepend writer for the happy path.
//
// "Equivalent" here means: same SSE events in the same order, with the
// notice injected at index 0 and upstream blocks shifted to index 1+.
// Byte-for-byte equality across implementations isn't expected because
// the legacy writer formats JSON with one shape and the new path uses
// json.Marshal (which sorts keys differently for synthesized blocks).
// What matters is structural equivalence: the parsed events match.
func TestPrependAnthropicAssistantNotice_OutputMatchesLegacy(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_x","role":"assistant","model":"claude-sonnet-4"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	const notice = "[Clawvisor] notice"

	// New stream-based path.
	var newBuf bytes.Buffer
	if err := stream.PrependAnthropicAssistantNotice(&newBuf, strings.NewReader(upstream), notice); err != nil {
		t.Fatalf("PrependAnthropicAssistantNotice: %v", err)
	}

	// Legacy path (existing conversation.NewStreamingFirstTurnNoticeWriter).
	var legacyBuf bytes.Buffer
	w := conversation.NewStreamingFirstTurnNoticeWriter(&legacyBuf, conversation.StreamShapeAnthropicMessages, notice)
	if _, err := w.Write([]byte(upstream)); err != nil {
		t.Fatalf("legacy Write: %v", err)
	}
	if closer, ok := w.(interface{ Close() error }); ok {
		_ = closer.Close()
	}

	// Both paths must surface the notice text exactly once.
	if c := strings.Count(newBuf.String(), notice); c != 1 {
		t.Errorf("new path: expected notice exactly once, got %d:\n%s", c, newBuf.String())
	}
	if c := strings.Count(legacyBuf.String(), notice); c != 1 {
		t.Errorf("legacy path: expected notice exactly once, got %d:\n%s", c, legacyBuf.String())
	}

	// Notice must appear BEFORE "hello" in both outputs.
	if i := strings.Index(newBuf.String(), notice); i < 0 || i >= strings.Index(newBuf.String(), "hello") {
		t.Errorf("new path: notice not before hello:\n%s", newBuf.String())
	}
	if i := strings.Index(legacyBuf.String(), notice); i < 0 || i >= strings.Index(legacyBuf.String(), "hello") {
		t.Errorf("legacy path: notice not before hello:\n%s", legacyBuf.String())
	}

	// The upstream blocks must shift to index 1 in both outputs.
	if !strings.Contains(newBuf.String(), `"index":1`) {
		t.Errorf("new path: upstream block index didn't shift to 1:\n%s", newBuf.String())
	}
	if !strings.Contains(legacyBuf.String(), `"index":1`) {
		t.Errorf("legacy path: upstream block index didn't shift to 1:\n%s", legacyBuf.String())
	}

	// Upstream text "hello" must survive verbatim — the surrounding
	// content_block_delta bytes for that event were PATCHED (index 0→1)
	// not REPLACED, so the rest of the bytes (including "hello") must
	// be unchanged.
	if !strings.Contains(newBuf.String(), `"text":"hello"`) {
		t.Errorf("new path: hello text didn't survive:\n%s", newBuf.String())
	}
}

// TestPrependAnthropicAssistantNotice_BlankIsCopy verifies the blank-text
// short-circuit: an empty notice copies the stream verbatim.
func TestPrependAnthropicAssistantNotice_BlankIsCopy(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start"}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var buf bytes.Buffer
	if err := stream.PrependAnthropicAssistantNotice(&buf, strings.NewReader(upstream), ""); err != nil {
		t.Fatalf("PrependAnthropicAssistantNotice blank: %v", err)
	}
	if got := buf.String(); got != upstream {
		t.Fatalf("blank notice should copy verbatim\n--- want ---\n%s\n--- got ---\n%s", upstream, got)
	}
}

func TestPrependAnthropicAssistantNotice_DefersPastThinkingBlock(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_x","role":"assistant","model":"claude-sonnet-4"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"sig"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	const notice = "[Clawvisor] notice"

	var buf bytes.Buffer
	if err := stream.PrependAnthropicAssistantNotice(&buf, strings.NewReader(upstream), notice); err != nil {
		t.Fatalf("PrependAnthropicAssistantNotice: %v", err)
	}
	got := buf.String()
	if thinking := strings.Index(got, `"type":"thinking"`); thinking < 0 || thinking > strings.Index(got, notice) {
		t.Fatalf("notice should be inserted after leading thinking block:\n%s", got)
	}
	if !strings.Contains(got, `"signature":"sig"`) {
		t.Fatalf("thinking signature was not preserved:\n%s", got)
	}
	if !strings.Contains(got, `"index":1`) || !strings.Contains(got, notice) {
		t.Fatalf("notice should be emitted at index 1:\n%s", got)
	}
	if !strings.Contains(got, `"index":2`) || !strings.Contains(got, `"text":"hello"`) {
		t.Fatalf("text block should shift to index 2:\n%s", got)
	}
}

func TestPrependAnthropicAssistantNotice_DefersPastRedactedThinkingBlock(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_x","role":"assistant","model":"claude-sonnet-4"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"encrypted"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	const notice = "[Clawvisor] notice"

	var buf bytes.Buffer
	if err := stream.PrependAnthropicAssistantNotice(&buf, strings.NewReader(upstream), notice); err != nil {
		t.Fatalf("PrependAnthropicAssistantNotice: %v", err)
	}
	got := buf.String()
	if thinking := strings.Index(got, `"type":"redacted_thinking"`); thinking < 0 || thinking > strings.Index(got, notice) {
		t.Fatalf("notice should be inserted after leading redacted_thinking block:\n%s", got)
	}
	if !strings.Contains(got, `"data":"encrypted"`) {
		t.Fatalf("redacted thinking payload was not preserved:\n%s", got)
	}
	if !strings.Contains(got, `"index":1`) || !strings.Contains(got, notice) {
		t.Fatalf("notice should be emitted at index 1:\n%s", got)
	}
	if !strings.Contains(got, `"index":2`) || !strings.Contains(got, `"text":"hello"`) {
		t.Fatalf("text block should shift to index 2:\n%s", got)
	}
}

func TestPrependAnthropicAssistantNotice_DoesNotAddMissingIndex(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-4"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	var buf strings.Builder
	if err := stream.PrependAnthropicAssistantNotice(&buf, strings.NewReader(upstream), "[Clawvisor] notice"); err != nil {
		t.Fatalf("PrependAnthropicAssistantNotice: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, `"text":"hello","index"`) || strings.Contains(got, `"index":1,"delta":{"type":"text_delta","text":"hello"}`) {
		t.Fatalf("missing upstream index should not be synthesized:\n%s", got)
	}
}

func TestPrependAnthropicAssistantNotice_NoMessageStartDoesNotAppendNotice(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var buf bytes.Buffer
	if err := stream.PrependAnthropicAssistantNotice(&buf, strings.NewReader(upstream), "[Clawvisor] notice"); err != nil {
		t.Fatalf("PrependAnthropicAssistantNotice: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "[Clawvisor] notice") {
		t.Fatalf("notice must not be appended after message_stop without message_start:\n%s", got)
	}
	if got != upstream {
		t.Fatalf("malformed stream should pass through unchanged\n--- want ---\n%s\n--- got ---\n%s", upstream, got)
	}
}
