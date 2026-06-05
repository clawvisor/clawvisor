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

// TestOpenAIChatRoundTrip_ByteIdentical pins byte-fidelity for the
// OpenAI Chat Completions stream shape.
func TestOpenAIChatRoundTrip_ByteIdentical(t *testing.T) {
	cases := []struct {
		name string
		sse  string
	}{
		{
			name: "simple text turn",
			sse: strings.Join([]string{
				`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
				``,
				`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
				``,
				`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				``,
				`data: [DONE]`,
				``,
			}, "\n"),
		},
		{
			name: "with SSE comment keepalive",
			sse: strings.Join([]string{
				`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
				``,
				`: vendor-ping`,
				`data: [DONE]`,
				``,
			}, "\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTripOpenAIChat(t, tc.sse)
			if got != tc.sse {
				t.Fatalf("round-trip not byte-identical\n--- want ---\n%q\n--- got ---\n%q", tc.sse, got)
			}
		})
	}
}

// TestOpenAIChatEncoder_PatchedTopLevelField verifies the PATCHED
// state on a chat chunk: editing one top-level field preserves all
// other bytes.
func TestOpenAIChatEncoder_PatchedTopLevelField(t *testing.T) {
	original := strings.Join([]string{
		`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		``,
	}, "\n")

	d := stream.NewOpenAIChatDecoder(strings.NewReader(original))
	ev, err := d.Next()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Patch: replace the `id` field.
	ev.FieldPatches = []stream.FieldPatch{{
		JSONPath: "id",
		NewValue: json.RawMessage(`"chatcmpl_patched"`),
	}}

	var buf bytes.Buffer
	enc := stream.NewOpenAIChatEncoder(&buf)
	if err := enc.Encode(ev); err != nil {
		t.Fatalf("encode: %v", err)
	}

	got := buf.String()
	// id must be patched.
	if !strings.Contains(got, `"id":"chatcmpl_patched"`) {
		t.Errorf("expected patched id, got:\n%s", got)
	}
	// All other content must survive verbatim — including the choices array.
	if !strings.Contains(got, `"choices":[{"index":0,"delta":{"content":"hi"}}]`) {
		t.Errorf("non-patched fields didn't survive verbatim:\n%s", got)
	}
}

func roundTripOpenAIChat(t *testing.T, sse string) string {
	t.Helper()
	d := stream.NewOpenAIChatDecoder(strings.NewReader(sse))
	var buf bytes.Buffer
	enc := stream.NewOpenAIChatEncoder(&buf)
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
