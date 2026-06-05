package stream_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation/stream"
)

// TestAnthropicRoundTrip_ByteIdentical pins the core byte-fidelity
// invariant: decoding an Anthropic SSE stream and re-encoding without
// mutations produces byte-identical output.
//
// This is the Phase 2 contract that protects thinking-block signatures.
// If this test fails, the proxy will silently corrupt thinking signatures
// on Anthropic responses — manifesting as 400 errors on the next turn.
func TestAnthropicRoundTrip_ByteIdentical(t *testing.T) {
	cases := []struct {
		name string
		sse  string
	}{
		{
			name: "simple text turn",
			sse: strings.Join([]string{
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
				`event: message_delta`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
			}, "\n"),
		},
		{
			name: "with thinking block",
			sse: strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_y","role":"assistant","model":"claude-sonnet-4"}}`,
				``,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
				``,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning..."}}`,
				``,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc123"}}`,
				``,
				`event: content_block_stop`,
				`data: {"type":"content_block_stop","index":0}`,
				``,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
				``,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ok"}}`,
				``,
				`event: content_block_stop`,
				`data: {"type":"content_block_stop","index":1}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
			}, "\n"),
		},
		{
			name: "with SSE keepalive comment",
			sse: strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_z","role":"assistant","model":"claude-sonnet-4"}}`,
				``,
				`: vendor-ping`,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				``,
				`event: content_block_stop`,
				`data: {"type":"content_block_stop","index":0}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
			}, "\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTripAnthropic(t, tc.sse)
			if got != tc.sse {
				t.Fatalf("round-trip not byte-identical\n--- want ---\n%s\n--- got ---\n%s", tc.sse, got)
			}
		})
	}
}

// TestAnthropicEncoder_PatchedIndexShiftOnly verifies the PATCHED state:
// an event marked with a single `index` FieldPatch should emit
// byte-identical bytes EXCEPT for the index value. All other fields
// (including the surrounding JSON shape) survive unchanged.
func TestAnthropicEncoder_PatchedIndexShiftOnly(t *testing.T) {
	original := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
	}, "\n")

	d := stream.NewAnthropicDecoder(strings.NewReader(original))
	ev, err := d.Next()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Meta.AnthropicIndex != 0 {
		t.Fatalf("expected AnthropicIndex=0, got %d", ev.Meta.AnthropicIndex)
	}

	// Patch: shift index 0 → 1.
	ev.FieldPatches = []stream.FieldPatch{{
		JSONPath: "index",
		NewValue: json.RawMessage(`1`),
	}}

	var buf bytes.Buffer
	enc := stream.NewAnthropicEncoder(&buf)
	if err := enc.Encode(ev); err != nil {
		t.Fatalf("encode: %v", err)
	}

	want := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
	}, "\n")
	if got := buf.String(); got != want {
		t.Fatalf("patched encode wrong\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestAnthropicEncoder_ReplacedTextBlock verifies the REPLACED state:
// a fully synthesized TextBlock for a content_block_delta event encodes
// to the expected SSE payload shape.
func TestAnthropicEncoder_ReplacedTextBlock(t *testing.T) {
	ev := stream.Event{
		Kind: stream.KindBlockDelta,
		Meta: stream.EventMeta{
			SSEEventName:   "content_block_delta",
			AnthropicIndex: 0,
		},
		Parsed: stream.TextBlock{Text: "hello"},
	}

	var buf bytes.Buffer
	enc := stream.NewAnthropicEncoder(&buf)
	if err := enc.Encode(ev); err != nil {
		t.Fatalf("encode: %v", err)
	}

	// The exact JSON-encoded payload (Go's encoder is deterministic for
	// these shapes).
	want := "event: content_block_delta\ndata: {\"delta\":{\"text\":\"hello\",\"type\":\"text_delta\"},\"index\":0,\"type\":\"content_block_delta\"}\n\n"
	if got := buf.String(); got != want {
		t.Fatalf("replaced encode wrong\n--- want ---\n%q\n--- got ---\n%q", want, got)
	}
}

// TestEvent_ValidateRejectsContractViolations pins the three-state
// invariant: setting both RawBytes and Parsed, or FieldPatches without
// RawBytes, must error.
func TestEvent_ValidateRejectsContractViolations(t *testing.T) {
	cases := []struct {
		name string
		ev   stream.Event
	}{
		{
			name: "both RawBytes and Parsed",
			ev: stream.Event{
				Kind:     stream.KindBlockDelta,
				RawBytes: []byte("event: foo\ndata: {}\n\n"),
				Parsed:   stream.TextBlock{Text: "x"},
			},
		},
		{
			name: "FieldPatches without RawBytes",
			ev: stream.Event{
				Kind:         stream.KindBlockStart,
				FieldPatches: []stream.FieldPatch{{JSONPath: "index", NewValue: json.RawMessage(`1`)}},
			},
		},
		{
			name: "FieldPatches with Parsed",
			ev: stream.Event{
				Kind:         stream.KindBlockDelta,
				FieldPatches: []stream.FieldPatch{{JSONPath: "index", NewValue: json.RawMessage(`1`)}},
				Parsed:       stream.TextBlock{Text: "x"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.ev.Validate(); err == nil {
				t.Fatalf("expected validation error, got nil")
			}
		})
	}
}

// roundTripAnthropic decodes input SSE through AnthropicDecoder, then
// encodes the un-mutated events back via AnthropicEncoder, returning
// the round-tripped bytes.
func roundTripAnthropic(t *testing.T, sse string) string {
	t.Helper()
	d := stream.NewAnthropicDecoder(strings.NewReader(sse))
	var buf bytes.Buffer
	enc := stream.NewAnthropicEncoder(&buf)
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if err := enc.Encode(ev); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	return buf.String()
}
