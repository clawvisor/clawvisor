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

// TestOpenAIResponsesRoundTrip_ByteIdentical pins byte-fidelity for the
// OpenAI Responses stream shape across canonical event sequences.
func TestOpenAIResponsesRoundTrip_ByteIdentical(t *testing.T) {
	cases := []struct {
		name string
		sse  string
	}{
		{
			name: "simple text turn",
			sse: strings.Join([]string{
				`event: response.created`,
				`data: {"type":"response.created","response":{"id":"resp_x","model":"gpt-5","output":[]}}`,
				``,
				`event: response.output_item.added`,
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress"}}`,
				``,
				`event: response.content_part.added`,
				`data: {"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
				``,
				`event: response.output_text.delta`,
				`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`,
				``,
				`event: response.output_text.done`,
				`data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"hello"}`,
				``,
				`event: response.content_part.done`,
				`data: {"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hello"}}`,
				``,
				`event: response.output_item.done`,
				`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello"}]}}`,
				``,
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"id":"resp_x"}}`,
				``,
			}, "\n"),
		},
		{
			name: "with keepalive",
			sse: strings.Join([]string{
				`event: response.created`,
				`data: {"type":"response.created","response":{"id":"resp_y"}}`,
				``,
				`: keepalive`,
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"id":"resp_y"}}`,
				``,
			}, "\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTripOpenAIResponses(t, tc.sse)
			if got != tc.sse {
				t.Fatalf("round-trip not byte-identical\n--- want ---\n%q\n--- got ---\n%q", tc.sse, got)
			}
		})
	}
}

// TestOpenAIResponsesDecoder_SurfacesIndexFields verifies that the
// decoder lifts output_index / content_index / item_id onto Meta so
// policies can issue FieldPatches against them by name instead of
// re-parsing JSON.
func TestOpenAIResponsesDecoder_SurfacesIndexFields(t *testing.T) {
	src := strings.Join([]string{
		`event: response.content_part.added`,
		`data: {"type":"response.content_part.added","item_id":"msg_abc","output_index":3,"content_index":1,"part":{"type":"output_text","text":""}}`,
		``,
	}, "\n")

	d := stream.NewOpenAIResponsesDecoder(strings.NewReader(src))
	ev, err := d.Next()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Meta.OpenAIOutputIndex != 3 {
		t.Errorf("OpenAIOutputIndex = %d, want 3", ev.Meta.OpenAIOutputIndex)
	}
	if ev.Meta.OpenAIContentIndex != 1 {
		t.Errorf("OpenAIContentIndex = %d, want 1", ev.Meta.OpenAIContentIndex)
	}
	if ev.Meta.OpenAIItemID != "msg_abc" {
		t.Errorf("OpenAIItemID = %q, want msg_abc", ev.Meta.OpenAIItemID)
	}
	if ev.Kind != stream.KindBlockStart {
		t.Errorf("Kind = %v, want KindBlockStart", ev.Kind)
	}
}

// TestOpenAIResponsesEncoder_PatchOutputIndex verifies PATCHED state
// for the most common operation: shifting output_index by +1 after a
// leading item is injected.
func TestOpenAIResponsesEncoder_PatchOutputIndex(t *testing.T) {
	original := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress"}}`,
		``,
	}, "\n")

	d := stream.NewOpenAIResponsesDecoder(strings.NewReader(original))
	ev, err := d.Next()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	ev.FieldPatches = []stream.FieldPatch{{
		JSONPath: "output_index",
		NewValue: json.RawMessage(`1`),
	}}

	var buf bytes.Buffer
	enc := stream.NewOpenAIResponsesEncoder(&buf)
	if err := enc.Encode(ev); err != nil {
		t.Fatalf("encode: %v", err)
	}

	want := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress"}}`,
		``,
	}, "\n")

	if got := buf.String(); got != want {
		t.Fatalf("patched encode wrong\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

func roundTripOpenAIResponses(t *testing.T, sse string) string {
	t.Helper()
	d := stream.NewOpenAIResponsesDecoder(strings.NewReader(sse))
	var buf bytes.Buffer
	enc := stream.NewOpenAIResponsesEncoder(&buf)
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
