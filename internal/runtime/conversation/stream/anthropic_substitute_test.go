package stream_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation/stream"
)

// TestSubstituteAnthropicResponse_EmitsSyntheticStream verifies the
// synthetic stream carries the substitute text in a single text block
// and drains the upstream source.
func TestSubstituteAnthropicResponse_EmitsSyntheticStream(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_upstream"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	const substitute = "Clawvisor paused this tool call for approval."

	var dst bytes.Buffer
	if err := stream.SubstituteAnthropicResponse(&dst, io.NopCloser(strings.NewReader(upstream)), substitute); err != nil {
		t.Fatalf("SubstituteAnthropicResponse: %v", err)
	}

	got := dst.String()

	// Upstream's tool_use must NOT appear in output (it's replaced).
	if strings.Contains(got, "toolu_1") {
		t.Errorf("upstream tool_use leaked into synthetic output:\n%s", got)
	}
	if strings.Contains(got, "msg_upstream") {
		t.Errorf("upstream message id leaked into synthetic output:\n%s", got)
	}

	// Substitute text appears in the content_block_delta.
	if !strings.Contains(got, substitute) {
		t.Errorf("substitute text missing from synthetic output:\n%s", got)
	}

	// Synthetic stream must carry the canonical event sequence.
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("synthetic stream missing canonical event %q:\n%s", want, got)
		}
	}

	// content_block_delta carries the substitute as text_delta.
	if !strings.Contains(got, `"type":"text_delta"`) {
		t.Errorf("substitute not in text_delta form:\n%s", got)
	}
}
