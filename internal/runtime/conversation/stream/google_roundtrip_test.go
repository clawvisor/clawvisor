package stream_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation/stream"
)

// TestGoogleRoundTrip_ByteIdentical pins byte-fidelity for the
// Gemini SSE stream shape — the same property that protects
// Anthropic thinking-block signatures applies here for any future
// signed content.
func TestGoogleRoundTrip_ByteIdentical(t *testing.T) {
	cases := []struct {
		name string
		sse  string
	}{
		{
			name: "single chunk",
			sse: strings.Join([]string{
				`data: {"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}]}`,
				``,
			}, "\n"),
		},
		{
			name: "multiple chunks",
			sse: strings.Join([]string{
				`data: {"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}]}`,
				``,
				`data: {"candidates":[{"content":{"parts":[{"text":" world"}],"role":"model"}}]}`,
				``,
				`data: {"candidates":[{"content":{"parts":[{"text":""}],"role":"model"},"finishReason":"STOP"}]}`,
				``,
			}, "\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := stream.NewGoogleDecoder(strings.NewReader(tc.sse))
			var buf bytes.Buffer
			enc := stream.NewGoogleEncoder(&buf)
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
			if got := buf.String(); got != tc.sse {
				t.Fatalf("round-trip not byte-identical\n--- want ---\n%s\n--- got ---\n%s", tc.sse, got)
			}
		})
	}
}

func TestGoogleDecoder_IgnoresCommentInsideEventRawBytes(t *testing.T) {
	src := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}]}`,
		`: vendor-ping`,
		`retry: 1000`,
		``,
	}, "\n")
	d := stream.NewGoogleDecoder(strings.NewReader(src))
	ev, err := d.Next()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Contains(string(ev.RawBytes), "vendor-ping") || strings.Contains(string(ev.RawBytes), "retry:") {
		t.Fatalf("event RawBytes included non-event lines: %q", ev.RawBytes)
	}
}
