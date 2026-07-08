package scenarios_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// stubVerifier is a tiny Anthropic-compatible upstream that returns a
// scripted verifier verdict — used in place of a real Anthropic call.
// Shared by intent-verify and audit scenarios.
type stubVerifier struct {
	mu      sync.Mutex
	srv     *httptest.Server
	calls   int
	verdict string // JSON the verifier expects inside content[0].text
}

func newStubVerifier(t *testing.T, verdictJSON string) *stubVerifier {
	t.Helper()
	v := &stubVerifier{verdict: verdictJSON}
	v.srv = httptest.NewServer(http.HandlerFunc(v.handle))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *stubVerifier) URL() string { return v.srv.URL }
func (v *stubVerifier) Calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

func (v *stubVerifier) handle(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	v.calls++
	verdict := v.verdict
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	// Anthropic Messages API response shape — verifier reads
	// content[0].text and parses it as JSON.
	resp := map[string]any{
		"id":          "msg_verifier_stub",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-haiku-4-5-20251001",
		"content":     []map[string]any{{"type": "text", "text": verdict}},
		"stop_reason": "end_turn",
		"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
	}
	_ = json.NewEncoder(w).Encode(resp)
}
