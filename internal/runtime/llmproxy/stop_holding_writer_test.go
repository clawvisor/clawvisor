package llmproxy

import (
	"bytes"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestStopHoldingWriter_BuffersTerminalMarker(t *testing.T) {
	var out bytes.Buffer
	w := newStopHoldingWriter(&out, conversation.ProviderAnthropic)

	if _, err := w.Write([]byte("event: content_block_stop\ndata: {}\n\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("event: message_stop\ndata: {}\n\n")); err != nil {
		t.Fatal(err)
	}
	// Before Flush the held bytes shouldn't be in out.
	if strings.Contains(out.String(), "message_stop") {
		t.Errorf("terminal marker leaked before Flush:\n%s", out.String())
	}
	// Caller writes the substitution to the inner writer directly.
	out.WriteString("inserted by drift resolver\n")
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	subIdx := strings.Index(got, "inserted by drift resolver")
	stopIdx := strings.Index(got, "event: message_stop")
	if subIdx < 0 || stopIdx < 0 {
		t.Fatalf("missing markers — sub=%d stop=%d full=%q", subIdx, stopIdx, got)
	}
	if subIdx > stopIdx {
		t.Errorf("substitution landed after marker (sub=%d stop=%d) — wrapper failed to hold marker", subIdx, stopIdx)
	}
}

// Cubic finding 3344888557: a terminal marker that straddles two
// Write calls (e.g. upstream chunk boundary lands in the middle of
// `event: message_stop`) must still be detected and held back. The
// writer speculatively buffers the trailing (maxMarkerLen-1) bytes
// of every write so the next chunk's prefix can complete a marker
// that began earlier. Once a marker IS detected, the substitution
// block can be appended to inner directly, and Flush emits the held
// marker tail after.
func TestStopHoldingWriter_DetectsMarkerSplitAcrossWrites(t *testing.T) {
	cases := []struct {
		name   string
		writes []string
	}{
		{name: "split at middle of word", writes: []string{"some content\nevent: messa", "ge_stop\ndata: {}\n\n"}},
		{name: "split at last byte", writes: []string{"abc\nevent: message_sto", "p\ndata: {}\n\n"}},
		{name: "split right at start of marker", writes: []string{"abc\n", "event: message_stop\ndata: {}\n\n"}},
		{name: "split into many tiny chunks", writes: []string{"a", "b", "c", "\n", "event:", " ", "mes", "sage_", "stop", "\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			w := newStopHoldingWriter(&out, conversation.ProviderAnthropic)
			for _, chunk := range tc.writes {
				if _, err := w.Write([]byte(chunk)); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}
			// Marker must NOT have leaked to inner before Flush.
			if strings.Contains(out.String(), "event: message_stop") {
				t.Errorf("marker leaked through to inner before Flush:\n%s", out.String())
			}
			// Caller writes appended substitution to inner.
			out.WriteString("--SUB--")
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			subIdx := strings.Index(got, "--SUB--")
			stopIdx := strings.Index(got, "event: message_stop")
			if subIdx < 0 || stopIdx < 0 {
				t.Fatalf("missing markers — sub=%d stop=%d full=%q", subIdx, stopIdx, got)
			}
			if subIdx > stopIdx {
				t.Errorf("substitution landed after marker (sub=%d stop=%d) — split-marker detection failed\nfull=%q", subIdx, stopIdx, got)
			}
		})
	}
}

func TestStopHoldingWriter_OpenAIMarkerSplitAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	w := newStopHoldingWriter(&out, conversation.ProviderOpenAI)
	// Split "data: [DONE]" across two Writes.
	if _, err := w.Write([]byte("some data\ndata: [DO")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("NE]\n\n")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "[DONE]") {
		t.Errorf("OpenAI [DONE] marker leaked before Flush:\n%s", out.String())
	}
	out.WriteString("appended-text\n")
	_ = w.Flush()
	got := out.String()
	if !strings.Contains(got, "[DONE]") {
		t.Errorf("marker missing after Flush: %q", got)
	}
	if strings.Index(got, "appended-text") > strings.Index(got, "[DONE]") {
		t.Errorf("appended-text landed after [DONE] — split-marker detection failed")
	}
}

// PostprocessStream's text-only early-return uses a deferred Flush so
// the buffered terminal marker always lands on the wire, even when a
// downstream Write fails. This test exercises just the Flush
// semantics via a small fixture; the integration tests in
// scope_drift_streaming_test.go cover the end-to-end behavior.
func TestStopHoldingWriter_FlushIsIdempotent(t *testing.T) {
	var out bytes.Buffer
	w := newStopHoldingWriter(&out, conversation.ProviderAnthropic)
	if _, err := w.Write([]byte("event: message_stop\ndata: {}\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	first := out.String()
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if first != out.String() {
		t.Errorf("Flush not idempotent: first=%q second=%q", first, out.String())
	}
}
