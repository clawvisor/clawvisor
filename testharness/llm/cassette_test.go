package llm_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	hllm "github.com/clawvisor/clawvisor/testharness/llm"
)

// TestRecordThenReplay validates the basic record→replay loop: hit a real
// upstream once in record mode, then replay deterministically without
// touching the upstream.
func TestRecordThenReplay(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_123","role":"assistant","content":[{"type":"text","text":"hello"}]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()

	// Record.
	rec := hllm.NewCassette(dir, "t", hllm.ModeRecord)
	rec.SetUpstream(http.DefaultTransport)
	resp, err := rec.Client().Post(upstream.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if hits != 1 {
		t.Fatalf("expected 1 upstream hit during record, got %d", hits)
	}
	if !strings.Contains(string(body), "msg_123") {
		t.Fatalf("body=%s", body)
	}

	// Replay (different cassette instance, same dir).
	rep := hllm.NewCassette(dir, "t", hllm.ModeReplay)
	resp2, err := rep.Client().Post(upstream.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if hits != 1 {
		t.Fatalf("upstream hit during replay (got %d total)", hits)
	}
	if !bytes.Equal(body, body2) {
		t.Fatalf("replay body mismatch:\nrec: %s\nrep: %s", body, body2)
	}

	// One cassette file was written.
	entries, _ := filepath.Glob(filepath.Join(dir, "t", "*.json"))
	if len(entries) != 1 {
		t.Fatalf("expected 1 cassette file, got %d", len(entries))
	}
}

// TestNormalizedBodyMatch validates that the same logical JSON in different
// key order matches the same cassette entry.
func TestNormalizedBodyMatch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	rec := hllm.NewCassette(dir, "t", hllm.ModeRecord)
	if _, err := rec.Client().Post(upstream.URL, "application/json",
		strings.NewReader(`{"a":1,"b":2}`)); err != nil {
		t.Fatal(err)
	}

	// Different key order in replay request — should still match.
	rep := hllm.NewCassette(dir, "t", hllm.ModeReplay)
	if _, err := rep.Client().Post(upstream.URL, "application/json",
		strings.NewReader(`{"b":2,"a":1}`)); err != nil {
		t.Fatalf("replay should match despite key reorder: %v", err)
	}
}
