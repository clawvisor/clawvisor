package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
)

// TestPostprocessStream_AppendsDriftSubstitutionForTextOnlyTurn drives
// PostprocessStream end-to-end with a synthetic Anthropic SSE response
// whose only content is text containing a <clawvisor:decision> markup
// block. Asserts that:
//
//   - the registry sees the drift get claimed (one_off path)
//   - the output stream contains the proxy's appended status text
//   - the output stream still ends with a well-formed message_stop
//
// Reproduces the failing live-test condition: the harness driver's
// classifier looks at the FINAL text block in the assembled SSE
// response, and unless the proxy's substitution lands BEFORE the
// terminal marker the harness drops it.
func TestPostprocessStream_AppendsDriftSubstitutionForTextOnlyTurn(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	pending := NewMemoryPendingApprovalCache(0)
	ctx := context.Background()

	drift, _ := reg.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		UserID:  "user-1",
		Service: "github",
		Action:  "add_issue_comment",
		Source:  ScopeDriftSourceIntentVerification,
	})

	cfg := PostprocessConfig{
		AgentID:          "agent-1",
		AgentUserID:      "user-1",
		ScopeDrifts:      reg,
		PendingApprovals: pending,
		Inspector:        inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
	}

	// Build a synthetic Anthropic SSE stream: message_start, one text
	// content block whose body is the agent's markup, then
	// message_stop. Mirrors how claude's API streams a text-only
	// assistant response. The text field is constructed via
	// json.Marshal so embedded quotes in the markup are properly
	// JSON-escaped (matching the wire shape claude's API produces).
	text := "You asked for a one-off:\n\n" +
		`<clawvisor:decision drift="` + drift.ID + `" option="one-off">Single courtesy comment on one issue.</clawvisor:decision>`
	deltaPayload, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	})
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[]}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: ` + string(deltaPayload),
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	var out bytes.Buffer
	_, err := PostprocessStream(ctx, req, strings.NewReader(sse), &out, "text/event-stream", cfg)
	if err != nil {
		t.Fatalf("PostprocessStream: %v", err)
	}
	body := out.String()

	// The agent's markup line should still be present (we don't mutate
	// the streamed deltas).
	if !strings.Contains(body, "clawvisor:decision") {
		t.Errorf("agent's markup was dropped from the output stream")
	}
	// The proxy's appended substitution must appear, AND it must come
	// before message_stop.
	if !strings.Contains(body, "Clawvisor: the agent requested a one-off execution") {
		t.Errorf("output missing appended drift substitution")
	}
	subIdx := strings.Index(body, "Clawvisor: the agent requested a one-off execution")
	stopIdx := strings.Index(body, "event: message_stop")
	if subIdx < 0 || stopIdx < 0 {
		t.Fatalf("missing markers — sub=%d stop=%d", subIdx, stopIdx)
	}
	if subIdx > stopIdx {
		t.Errorf("substitution lands after message_stop (sub=%d stop=%d) — claude CLI drops it as out-of-band", subIdx, stopIdx)
	}

	// Registry state: drift claimed as one_off, pending hold opened.
	updated, _ := reg.Get(ctx, drift.ID)
	if updated.ChosenOption != ScopeDriftOptionOneOff {
		t.Errorf("expected ChosenOption=one_off, got %q", updated.ChosenOption)
	}
	holds := snapshotPendingApprovals(pending, "user-1", "agent-1", conversation.ProviderAnthropic)
	if len(holds) != 1 {
		t.Fatalf("expected 1 pending hold, got %d", len(holds))
	}
}
