package llm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	hllm "github.com/clawvisor/clawvisor/testharness/llm"
)

// TestAnthropicLiveRecordReplay hits the real Anthropic API once to record
// a cassette, then replays it deterministically. Skipped when
// ANTHROPIC_API_KEY isn't set. Validates the cassette layer against the real
// Anthropic Messages API wire shape.
func TestAnthropicLiveRecordReplay(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	dir := t.TempDir()

	body := []byte(`{
  "model": "claude-haiku-4-5-20251001",
  "max_tokens": 16,
  "messages": [{"role": "user", "content": "Reply with the word OK and nothing else."}]
}`)

	// Record one real call.
	rec := hllm.NewCassette(dir, t.Name(), hllm.ModeRecord)
	rec.SetUpstream(http.DefaultTransport)
	req, _ := http.NewRequestWithContext(context.Background(), "POST",
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := rec.Client().Do(req)
	if err != nil {
		t.Fatalf("record live call: %v", err)
	}
	recBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read record body: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("anthropic status=%d body=%s", resp.StatusCode, recBody)
	}

	// Confirm structurally — content[0].text exists.
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(recBody, &parsed); err != nil {
		t.Fatalf("parse anthropic response: %v\n%s", err, recBody)
	}
	if len(parsed.Content) == 0 || parsed.Content[0].Type != "text" {
		t.Fatalf("unexpected response shape: %s", recBody)
	}

	// Now replay without hitting the network — re-send the same request and
	// verify the body matches.
	rep := hllm.NewCassette(dir, t.Name(), hllm.ModeReplay)
	req2, _ := http.NewRequestWithContext(context.Background(), "POST",
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-api-key", "FAKE-KEY-NOT-VALID") // replay shouldn't care
	req2.Header.Set("anthropic-version", "2023-06-01")
	resp2, err := rep.Client().Do(req2)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	repBody, err := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if err != nil {
		t.Fatalf("read replay body: %v", err)
	}
	if !bytes.Equal(recBody, repBody) {
		t.Fatalf("replay body mismatch")
	}

	// Cassette file present on disk.
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.json"))
	if len(files) != 1 {
		t.Fatalf("expected 1 cassette file on disk, got %d (%v)", len(files), files)
	}
}
