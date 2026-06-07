package llmproxy

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// ProgressReader must be a transparent pass-through — the data path
// cannot be perturbed by instrumentation.
func TestProgressReader_PassesBytesUnchanged(t *testing.T) {
	want := strings.Repeat("hello world ", 1000)
	src := bytes.NewReader([]byte(want))
	var buf bytes.Buffer
	pr := NewProgressReader(src, nil, nil, "req-pr")
	if _, err := io.Copy(&buf, pr); err != nil {
		t.Fatal(err)
	}
	if buf.String() != want {
		t.Errorf("bytes mutated by ProgressReader")
	}
}

func TestProgressReader_StatsCountReadBytes(t *testing.T) {
	src := bytes.NewReader([]byte("0123456789"))
	pr := NewProgressReader(src, nil, nil, "req-pr-stats")
	var out [10]byte
	if _, err := io.ReadFull(pr, out[:]); err != nil {
		t.Fatal(err)
	}
	bytesTotal, elapsed, ttfb := pr.Stats()
	if bytesTotal != 10 {
		t.Errorf("bytes_total=%d, want 10", bytesTotal)
	}
	if ttfb > elapsed {
		t.Errorf("ttfb=%v elapsed=%v — ttfb should be <= elapsed", ttfb, elapsed)
	}
	if pr.firstByte.IsZero() {
		t.Error("firstByte timestamp was not recorded")
	}
}

func TestProgressReader_PropagatesError(t *testing.T) {
	want := errors.New("boom")
	pr := NewProgressReader(&erroringReader{err: want}, nil, nil, "req-err")
	var buf [16]byte
	_, err := pr.Read(buf[:])
	if !errors.Is(err, want) {
		t.Errorf("err=%v, want %v", err, want)
	}
}

type erroringReader struct{ err error }

func (e *erroringReader) Read(_ []byte) (int, error) { return 0, e.err }
