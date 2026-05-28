package conversation

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSyntheticErrorResponseAnthropicStreaming(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	body := []byte(`{"model":"claude-opus-4-7","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	got, ok := SyntheticErrorResponse(req, ProviderAnthropic, body, "approval expired, please retry")
	if !ok {
		t.Fatal("expected ok=true for Anthropic streaming request")
	}
	if got.ContentType != "text/event-stream" {
		t.Fatalf("ContentType = %q, want text/event-stream", got.ContentType)
	}
	s := string(got.Body)
	if !strings.Contains(s, "event: message_start") {
		t.Fatalf("body missing message_start event:\n%s", s)
	}
	if !strings.Contains(s, "event: message_stop") {
		t.Fatalf("body missing message_stop event:\n%s", s)
	}
	if !strings.Contains(s, "approval expired, please retry") {
		t.Fatalf("body missing error message:\n%s", s)
	}
}

func TestSyntheticErrorResponseAnthropicNonStreaming(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	got, ok := SyntheticErrorResponse(req, ProviderAnthropic, body, "upstream request failed")
	if !ok {
		t.Fatal("expected ok=true for Anthropic non-streaming request")
	}
	if got.ContentType != "application/json" {
		t.Fatalf("ContentType = %q, want application/json", got.ContentType)
	}
	s := string(got.Body)
	if !strings.Contains(s, `"type":"message"`) {
		t.Fatalf("body missing message type:\n%s", s)
	}
	if !strings.Contains(s, `"role":"assistant"`) {
		t.Fatalf("body missing assistant role:\n%s", s)
	}
	if !strings.Contains(s, "upstream request failed") {
		t.Fatalf("body missing error message:\n%s", s)
	}
}

func TestSyntheticErrorResponseOpenAIChatStreaming(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/v1/chat/completions", nil)
	body := []byte(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	got, ok := SyntheticErrorResponse(req, ProviderOpenAI, body, "approval not found")
	if !ok {
		t.Fatal("expected ok=true for OpenAI Chat streaming request")
	}
	if got.ContentType != "text/event-stream" {
		t.Fatalf("ContentType = %q, want text/event-stream", got.ContentType)
	}
	s := string(got.Body)
	if !strings.Contains(s, "data:") {
		t.Fatalf("body missing SSE data lines:\n%s", s)
	}
	if !strings.Contains(s, "approval not found") {
		t.Fatalf("body missing error message:\n%s", s)
	}
}

func TestSyntheticErrorResponseOpenAIChatNonStreaming(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/v1/chat/completions", nil)
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	got, ok := SyntheticErrorResponse(req, ProviderOpenAI, body, "approval not found")
	if !ok {
		t.Fatal("expected ok=true for OpenAI Chat non-streaming request")
	}
	if got.ContentType != "application/json" {
		t.Fatalf("ContentType = %q, want application/json", got.ContentType)
	}
	s := string(got.Body)
	if !strings.Contains(s, `"choices"`) {
		t.Fatalf("body missing choices array:\n%s", s)
	}
	if !strings.Contains(s, "approval not found") {
		t.Fatalf("body missing error message:\n%s", s)
	}
}

func TestSyntheticErrorResponseOpenAIResponsesStreaming(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/v1/responses", nil)
	body := []byte(`{"model":"gpt-5","stream":true,"input":"hi"}`)
	got, ok := SyntheticErrorResponse(req, ProviderOpenAI, body, "decision input unavailable")
	if !ok {
		t.Fatal("expected ok=true for OpenAI Responses streaming request")
	}
	if got.ContentType != "text/event-stream" {
		t.Fatalf("ContentType = %q, want text/event-stream", got.ContentType)
	}
	s := string(got.Body)
	if !strings.Contains(s, "decision input unavailable") {
		t.Fatalf("body missing error message:\n%s", s)
	}
}

func TestSyntheticErrorResponseOpenAIResponsesNonStreaming(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/v1/responses", nil)
	body := []byte(`{"model":"gpt-5","input":"hi"}`)
	got, ok := SyntheticErrorResponse(req, ProviderOpenAI, body, "decision input unavailable")
	if !ok {
		t.Fatal("expected ok=true for OpenAI Responses non-streaming request")
	}
	if got.ContentType != "application/json" {
		t.Fatalf("ContentType = %q, want application/json", got.ContentType)
	}
	s := string(got.Body)
	if !strings.Contains(s, "decision input unavailable") {
		t.Fatalf("body missing error message:\n%s", s)
	}
}

func TestSyntheticErrorResponseEmptyMessage(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	body := []byte(`{}`)
	if _, ok := SyntheticErrorResponse(req, ProviderAnthropic, body, ""); ok {
		t.Fatal("expected ok=false for empty message")
	}
	if _, ok := SyntheticErrorResponse(req, ProviderAnthropic, body, "   "); ok {
		t.Fatal("expected ok=false for whitespace-only message")
	}
}

func TestSyntheticErrorResponseUnsupportedProvider(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	if _, ok := SyntheticErrorResponse(req, Provider("not-a-real-provider"), []byte(`{}`), "something failed"); ok {
		t.Fatal("expected ok=false for unsupported provider")
	}
}

func TestSyntheticErrorResponseMalformedBodyFallsBackToNonStreaming(t *testing.T) {
	// Stream detection probes the body for `"stream":true`; on malformed
	// JSON the probe returns false, so we should fall back to the
	// non-streaming JSON shape rather than emitting a half-baked SSE.
	// This matters because most pre-parse error sites (REQUEST_TOO_LARGE,
	// MALFORMED_REQUEST) call us with a body that may not be valid JSON.
	req := httptest.NewRequest("POST", "/api/v1/messages", nil)
	got, ok := SyntheticErrorResponse(req, ProviderAnthropic, []byte("not-json"), "request too large")
	if !ok {
		t.Fatal("expected ok=true even for malformed body")
	}
	if got.ContentType != "application/json" {
		t.Fatalf("ContentType = %q, want application/json fallback", got.ContentType)
	}
	if !strings.Contains(string(got.Body), "request too large") {
		t.Fatalf("body missing error message:\n%s", string(got.Body))
	}
}
