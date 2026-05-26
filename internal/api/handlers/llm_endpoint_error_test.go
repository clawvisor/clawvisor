package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
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
