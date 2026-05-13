package llmproxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTraceLogger_NilIsNoOp(t *testing.T) {
	var tr *TraceLogger
	tr.Emit(map[string]any{"event": "noop"}) // must not panic
}

func TestTraceLogger_EmitsJSONLine(t *testing.T) {
	var buf bytes.Buffer
	tr := NewTraceLogger(&buf)
	tr.now = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	tr.Emit(map[string]any{
		"event":     TraceEventInspectVerdict,
		"tool_name": "Bash",
		"reason":    "ok",
	})
	got := buf.String()
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("trace line must end with newline, got %q", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(strings.TrimRight(got, "\n")), &parsed); err != nil {
		t.Fatalf("trace line is not valid JSON: %v\n%s", err, got)
	}
	if parsed["event"] != TraceEventInspectVerdict {
		t.Errorf("event field missing/wrong: %v", parsed["event"])
	}
	if parsed["tool_name"] != "Bash" {
		t.Errorf("tool_name field missing/wrong: %v", parsed["tool_name"])
	}
	if parsed["timestamp"] != "2023-11-14T22:13:20Z" {
		t.Errorf("timestamp field missing/wrong: %v", parsed["timestamp"])
	}
}

func TestTraceLogger_ConcurrentSafe(t *testing.T) {
	var buf bytes.Buffer
	tr := NewTraceLogger(&buf)
	const N = 64
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			tr.Emit(map[string]any{"event": "concurrent", "i": i})
		}(i)
	}
	wg.Wait()
	// Every line must be valid JSON — i.e. the writes were serialized
	// and didn't interleave.
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("interleaved write produced invalid JSON: %v\nline=%q", err, line)
		}
	}
}

func TestTruncateForTrace(t *testing.T) {
	cases := []struct {
		in    string
		limit int
		want  string
	}{
		{"hello", 100, "hello"},
		{"hello world", 5, "hello...<truncated>"},
		{"", 10, ""},
		{"x", 0, "x"}, // limit 0 disables truncation
	}
	for _, tc := range cases {
		if got := truncateForTrace(tc.in, tc.limit); got != tc.want {
			t.Errorf("truncateForTrace(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}
	}
}
