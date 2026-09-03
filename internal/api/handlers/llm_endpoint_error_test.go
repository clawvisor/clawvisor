package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestWriteLiteProxyErrorAnthropicStreaming covers the bug that motivated
// PR 1: an expired inline approval used to return a non-harness-shaped
// JSON 404, which Claude Code surfaced as "model claude-opus-4-7[1m]
// may not exist." With writeLiteProxyError, the same error surfaces as
// an Anthropic SSE stream carrying the message as assistant text, which
// the harness renders inline so the user can retry.
func TestWriteLiteProxyErrorAnthropicStreaming(t *testing.T) {
	h := &LLMEndpointHandler{}
	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	rr := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	agent := &store.Agent{ID: "agent-1", UserID: "user-1"}

	h.writeLiteProxyError(rr, req, agent, conversation.ProviderAnthropic, body, "req-1",
		404, "APPROVAL_RELEASE_ERROR", "no matching pending approval; the approval may have expired")

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (harness-shaped responses always 200)", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	s := rr.Body.String()
	if !strings.Contains(s, "event: message_start") {
		t.Fatalf("body missing message_start:\n%s", s)
	}
	if !strings.Contains(s, "no matching pending approval") {
		t.Fatalf("body missing error message:\n%s", s)
	}
}

func TestWriteLiteProxyErrorAnthropicNonStreaming(t *testing.T) {
	h := &LLMEndpointHandler{}
	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	rr := httptest.NewRecorder()
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	agent := &store.Agent{ID: "agent-1", UserID: "user-1"}

	h.writeLiteProxyError(rr, req, agent, conversation.ProviderAnthropic, body, "req-1",
		502, "UPSTREAM_ERROR", "upstream request failed")

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	s := rr.Body.String()
	if !strings.Contains(s, `"type":"message"`) {
		t.Fatalf("body missing Anthropic JSON message shape:\n%s", s)
	}
	if !strings.Contains(s, "upstream request failed") {
		t.Fatalf("body missing error message:\n%s", s)
	}
}

func TestWriteLiteProxyErrorOpenAIChatStreaming(t *testing.T) {
	h := &LLMEndpointHandler{}
	req := httptest.NewRequest("POST", "/api/v1/chat/completions", nil)
	rr := httptest.NewRecorder()
	body := []byte(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	agent := &store.Agent{ID: "agent-1", UserID: "user-1"}

	h.writeLiteProxyError(rr, req, agent, conversation.ProviderOpenAI, body, "req-1",
		400, "MALFORMED_REQUEST", "could not parse request body")

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rr.Body.String(), "could not parse request body") {
		t.Fatalf("body missing error message:\n%s", rr.Body.String())
	}
}

// Provider with no synthesizer falls back to plain JSON. Today this path
// is unreachable in production (parser validation happens earlier in
// serve()), but the helper must not panic if called with one.
func TestWriteLiteProxyErrorUnsupportedProviderFallsBackToJSON(t *testing.T) {
	h := &LLMEndpointHandler{}
	req := httptest.NewRequest("POST", "/some/path", nil)
	rr := httptest.NewRecorder()
	agent := &store.Agent{ID: "agent-1", UserID: "user-1"}

	h.writeLiteProxyError(rr, req, agent, conversation.Provider("nope"), nil, "req-1",
		500, "UNKNOWN", "something failed")

	if rr.Code != 500 {
		t.Fatalf("status = %d, want 500 fallback", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rr.Body.String(), `"code":"UNKNOWN"`) {
		t.Fatalf("body missing JSON error code:\n%s", rr.Body.String())
	}
}

// Mirrored upstream headers (Content-Length, Anthropic-Request-Id, etc)
// must be cleared before writing the synthetic body. Without this,
// Content-Length leaks the upstream value and clients short-read our
// shorter synthetic body.
func TestWriteLiteProxyErrorClearsMirroredUpstreamHeaders(t *testing.T) {
	h := &LLMEndpointHandler{}
	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	rr := httptest.NewRecorder()
	// Simulate an in-flight upstream response that already mirrored headers.
	rr.Header().Set("Content-Length", "99999")
	rr.Header().Set("Anthropic-Request-Id", "upstream-request-id")
	body := []byte(`{"model":"claude-opus-4-7","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	agent := &store.Agent{ID: "agent-1", UserID: "user-1"}

	h.writeLiteProxyError(rr, req, agent, conversation.ProviderAnthropic, body, "req-1",
		502, "UPSTREAM_READ_ERROR", "upstream read failed")

	if cl := rr.Header().Get("Content-Length"); cl != "" {
		t.Fatalf("Content-Length leaked from upstream: %q", cl)
	}
	if id := rr.Header().Get("Anthropic-Request-Id"); id != "" {
		t.Fatalf("Anthropic-Request-Id leaked from upstream: %q", id)
	}
}

func TestLLMEndpoint_StreamErrorAfterHeaderCommit(t *testing.T) {
	// Set up an upstream server that starts sending a stream, flushes a few events,
	// and then abruptly closes the connection (simulating a mid-stream drop).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}

		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`))
		if ok {
			flusher.Flush()
		}

		// Abruptly close the connection using hijacking
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("webserver doesn't support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack failed: %v", err)
			return
		}
		_ = conn.Close() // Close connection abruptly
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)
	h.Inspector = inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))

	body := []byte(`{"model":"claude-sonnet-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// Since headers were committed, status code is 200.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	got := rec.Body.String()
	// Check that our WriteStreamError appended the error block to the stream.
	if !strings.Contains(got, "event: error") {
		t.Fatalf("expected event: error in output, got:\n%s", got)
	}
	if !strings.Contains(got, "The upstream connection was lost before the response completed") {
		t.Fatalf("expected Clawvisor error message in output, got:\n%s", got)
	}
}

