package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestStreamingPostprocessErrorFrame_AnthropicSplicesErrorEvent(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	frame, ok := streamingPostprocessErrorFrame(req, conversation.ProviderAnthropic, "lost stream")
	if !ok {
		t.Fatal("expected anthropic stream error frame")
	}
	got := string(frame)
	if strings.Contains(got, "message_start") {
		t.Fatalf("stream error frame started a new message: %s", got)
	}
	if !strings.Contains(got, "event: error") || !strings.Contains(got, "lost stream") {
		t.Fatalf("unexpected stream error frame: %s", got)
	}
	if !strings.Contains(got, "event: message_stop") {
		t.Fatalf("Anthropic stream error should terminate the message: %s", got)
	}
}

func TestStreamingPostprocessErrorFrame_OpenAIResponsesSplicesFailedEvent(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	frame, ok := streamingPostprocessErrorFrame(req, conversation.ProviderOpenAI, "lost stream")
	if !ok {
		t.Fatal("expected OpenAI Responses stream error frame")
	}
	got := string(frame)
	if strings.Contains(got, "response.created") {
		t.Fatalf("stream error frame started a new response: %s", got)
	}
	if !strings.Contains(got, "event: response.failed") {
		t.Fatalf("unexpected stream error frame: %s", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("OpenAI Responses stream error should terminate with DONE: %s", got)
	}
	if strings.Contains(got, "resp_clawvisor_error") {
		t.Fatalf("stream error frame should not invent a response id: %s", got)
	}
}

func TestStreamingPostprocessErrorFrame_OpenAIChatSplicesChatChunk(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	frame, ok := streamingPostprocessErrorFrame(req, conversation.ProviderOpenAI, "lost stream")
	if !ok {
		t.Fatal("expected OpenAI Chat stream error frame")
	}
	got := string(frame)
	if !strings.Contains(got, "chat.completion.chunk") || !strings.Contains(got, "lost stream") {
		t.Fatalf("unexpected OpenAI Chat stream error frame: %s", got)
	}
	if strings.Contains(got, `"error"`) {
		t.Fatalf("OpenAI Chat stream error should be a chat chunk, not generic error JSON: %s", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("OpenAI Chat stream error should terminate with DONE: %s", got)
	}
}

func TestStreamingPostprocessErrorFrame_GoogleSplicesErrorChunk(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:streamGenerateContent", nil)
	frame, ok := streamingPostprocessErrorFrame(req, conversation.ProviderGoogle, "lost stream")
	if !ok {
		t.Fatal("expected Google stream error frame")
	}
	got := string(frame)
	if !strings.Contains(got, "data:") || !strings.Contains(got, "lost stream") {
		t.Fatalf("unexpected stream error frame: %s", got)
	}
}
