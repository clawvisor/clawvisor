package llmproxy

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// shortWriter accepts only the first `accept` bytes of every Write
// then returns io.ErrShortWrite. Used to verify partial-write
// accounting in stopHoldingWriter.
type shortWriter struct {
	accept int
	got    bytes.Buffer
}

func (s *shortWriter) Write(p []byte) (int, error) {
	if len(p) <= s.accept {
		return s.got.Write(p)
	}
	if _, err := s.got.Write(p[:s.accept]); err != nil {
		return 0, err
	}
	return s.accept, errors.New("short write")
}

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
// Cubic finding 3345014107: Write must return an accurate byte count
// from p when inner.Write fails partway through forwarding. Returning
// (0, err) when bytes from p have already been absorbed (forwarded or
// buffered) violates the io.Writer contract: callers retrying from p[n:]
// after a short write would resend bytes already on the wire.
func TestStopHoldingWriter_WriteReportsBytesConsumedOnPartialFailure(t *testing.T) {
	// inner accepts only 5 bytes per call. Our Write feeds it a
	// chunk with no marker, so the writer tries to forward most of
	// the chunk; the inner fails after 5 bytes. The returned count
	// must reflect bytes-from-p actually consumed (forwarded or
	// pending), not 0.
	inner := &shortWriter{accept: 5}
	w := newStopHoldingWriter(inner, conversation.ProviderAnthropic)
	p := []byte("hello, world, this is not a marker")
	n, err := w.Write(p)
	if err == nil {
		t.Fatal("expected short-write error")
	}
	if n < 0 || n > len(p) {
		t.Errorf("n out of range: %d (len(p)=%d)", n, len(p))
	}
	// inner saw exactly the bytes it accepted; n should reflect at
	// most that count (less if pending absorbed some of p separately).
	if n > inner.accept {
		t.Errorf("n=%d > inner.accept=%d — writer over-reports bytes consumed", n, inner.accept)
	}
}

// Cubic finding 3345014110: Flush errors from the deferred flush
// must surface to the caller, not be silently dropped. A flush
// failure means the SSE stream lost its terminal marker — clients
// would hang waiting for message_stop / [DONE].
func TestPostprocessStream_PropagatesFlushErrorOnSuccess(t *testing.T) {
	// We can't easily exercise PostprocessStream's deferred flush
	// path from this layer (it needs the full Postprocess machinery),
	// but we can lock in the stopHoldingWriter.Flush contract: a
	// partial write inside Flush surfaces as an error.
	inner := &shortWriter{accept: 4}
	w := newStopHoldingWriter(inner, conversation.ProviderAnthropic)
	// Write a marker so the writer holds bytes.
	if _, err := w.Write([]byte("event: message_stop\ndata: {}\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err == nil {
		t.Errorf("Flush silently swallowed short-write error from inner")
	}
}

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
