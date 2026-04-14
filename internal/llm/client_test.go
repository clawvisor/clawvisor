package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
	"golang.org/x/oauth2"
)

// oaResponse builds a minimal OpenAI-compatible chat completions response.
func oaResponse(content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"content": content}},
		},
	})
	return b
}

// newLLMServer spins up a test HTTP server and returns an LLMProviderConfig pointing at it.
func newLLMServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, config.LLMProviderConfig) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, config.LLMProviderConfig{
		Enabled:        true,
		Endpoint:       ts.URL + "/v1",
		APIKey:         "test-key",
		Model:          "test-model",
		TimeoutSeconds: 5,
	}
}

func TestClient_Complete_Success(t *testing.T) {
	_, cfg := newLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected Authorization header: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(oaResponse("hello from llm"))
	})

	client := llm.NewClient(cfg)
	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("Complete: unexpected error: %v", err)
	}
	if got != "hello from llm" {
		t.Errorf("Complete: got %q, want %q", got, "hello from llm")
	}
}

func TestClient_Complete_SendsModelAndMessages(t *testing.T) {
	_, cfg := newLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["model"] != "test-model" {
			t.Errorf("model: got %v, want test-model", body["model"])
		}
		msgs, ok := body["messages"].([]any)
		if !ok || len(msgs) == 0 {
			t.Errorf("messages field missing or empty")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(oaResponse("ok"))
	})

	client := llm.NewClient(cfg)
	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_Complete_NonOKStatus(t *testing.T) {
	_, cfg := newLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error": "rate limited"}`))
	})

	client := llm.NewClient(cfg)
	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err == nil {
		t.Fatal("expected error for non-200 status")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected 429 in error, got: %v", err)
	}
}

func TestClient_Complete_EmptyChoices(t *testing.T) {
	_, cfg := newLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices": []}`))
	})

	client := llm.NewClient(cfg)
	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("expected 'no choices' in error, got: %v", err)
	}
}

func TestClient_Complete_InvalidJSON(t *testing.T) {
	_, cfg := newLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not-json`))
	})

	client := llm.NewClient(cfg)
	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestClient_Complete_DefaultTimeout(t *testing.T) {
	// TimeoutSeconds=0 → should use default (no panic, still works)
	_, cfg := newLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(oaResponse("ok"))
	})
	cfg.TimeoutSeconds = 0

	client := llm.NewClient(cfg)
	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "ping"},
	})
	if err != nil {
		t.Fatalf("default timeout: unexpected error: %v", err)
	}
	if got != "ok" {
		t.Errorf("default timeout: got %q, want ok", got)
	}
}

// ── Anthropic provider ────────────────────────────────────────────────────────

func anthropicResponse(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "message",
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	})
	return b
}

func newAnthropicServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, config.LLMProviderConfig) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, config.LLMProviderConfig{
		Enabled:        true,
		Provider:       "anthropic",
		Endpoint:       ts.URL + "/v1",
		APIKey:         "sk-ant-test",
		Model:          "claude-sonnet-4-6",
		TimeoutSeconds: 5,
	}
}

func TestClient_Anthropic_Complete_Success(t *testing.T) {
	_, cfg := newAnthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("anthropic: unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "sk-ant-test" {
			t.Errorf("anthropic: unexpected x-api-key: %s", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic: anthropic-version header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(anthropicResponse("hello from claude"))
	})

	client := llm.NewClient(cfg)
	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("anthropic Complete: unexpected error: %v", err)
	}
	if got != "hello from claude" {
		t.Errorf("anthropic Complete: got %q, want %q", got, "hello from claude")
	}
}

func TestClient_Anthropic_ExtractsSystemMessage(t *testing.T) {
	_, cfg := newAnthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		// System message must be in top-level "system" field, not in messages array.
		if body["system"] != "you are a policy generator" {
			t.Errorf("system field: got %v", body["system"])
		}
		msgs, _ := body["messages"].([]any)
		for _, m := range msgs {
			msg := m.(map[string]any)
			if msg["role"] == "system" {
				t.Error("system message should not appear in messages array")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(anthropicResponse("ok"))
	})

	client := llm.NewClient(cfg)
	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "you are a policy generator"},
		{Role: "user", Content: "generate a policy"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_Anthropic_NoSystemMessage(t *testing.T) {
	_, cfg := newAnthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		// When no system message, the "system" field should be absent (not empty string).
		if _, exists := body["system"]; exists {
			t.Errorf("system field should be absent when no system message, got: %v", body["system"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(anthropicResponse("ok"))
	})

	client := llm.NewClient(cfg)
	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hello"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_Anthropic_NonOKStatus(t *testing.T) {
	_, cfg := newAnthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": {"type": "authentication_error", "message": "invalid key"}}`))
	})

	client := llm.NewClient(cfg)
	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err == nil {
		t.Fatal("anthropic: expected error for 401 status")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("anthropic: expected 401 in error, got: %v", err)
	}
}

func TestClient_Anthropic_NoTextContent(t *testing.T) {
	_, cfg := newAnthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Response with only a non-text content block.
		resp, _ := json.Marshal(map[string]any{
			"type":    "message",
			"content": []map[string]any{{"type": "tool_use", "id": "x"}},
		})
		w.Write(resp)
	})

	client := llm.NewClient(cfg)
	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err == nil {
		t.Fatal("anthropic: expected error when no text content block")
	}
}

func TestClient_Anthropic_DefaultProviderIsOpenAI(t *testing.T) {
	// When Provider is empty, client should use OpenAI format (Authorization: Bearer).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("empty provider: expected OpenAI path, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("empty provider: expected Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(oaResponse("default"))
	}))
	t.Cleanup(ts.Close)

	cfg := config.LLMProviderConfig{
		Provider:       "", // empty → defaults to openai
		Endpoint:       ts.URL + "/v1",
		APIKey:         "key",
		Model:          "m",
		TimeoutSeconds: 5,
	}
	client := llm.NewClient(cfg)
	got, err := client.Complete(context.Background(), []llm.ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("empty provider: unexpected error: %v", err)
	}
	if got != "default" {
		t.Errorf("empty provider: got %q, want default", got)
	}
}

// ── Vertex AI fallback ────────────────────────────────────────────────────────

// staticToken implements oauth2.TokenSource for testing.
type staticToken struct{}

func (staticToken) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: "fake-token"}, nil
}

func newVertexClient(t *testing.T, endpoint string, fallbacks ...string) *llm.Client {
	t.Helper()
	cfg := config.LLMProviderConfig{
		Enabled:        true,
		Provider:       "vertex",
		Endpoint:       endpoint,
		Model:          "claude-haiku-4-5-20251001",
		TimeoutSeconds: 5,
	}
	c := llm.NewClient(cfg)
	c = c.WithTokenSource(staticToken{})
	if len(fallbacks) > 0 {
		c = c.WithFallbackEndpoints(fallbacks)
	}
	return c
}

func TestClient_Vertex_FallbackOnServerError(t *testing.T) {
	// Primary returns 503, fallback returns success.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error": "region down"}`))
	}))
	t.Cleanup(primary.Close)

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(anthropicResponse("from fallback"))
	}))
	t.Cleanup(fallback.Close)

	client := newVertexClient(t, primary.URL, fallback.URL)
	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("expected fallback success, got error: %v", err)
	}
	if got != "from fallback" {
		t.Errorf("got %q, want %q", got, "from fallback")
	}
}

func TestClient_Vertex_FallbackOn429(t *testing.T) {
	// Primary returns 429, fallback returns success.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error": "rate limited"}`))
	}))
	t.Cleanup(primary.Close)

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(anthropicResponse("fallback ok"))
	}))
	t.Cleanup(fallback.Close)

	client := newVertexClient(t, primary.URL, fallback.URL)
	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("expected fallback success, got error: %v", err)
	}
	if got != "fallback ok" {
		t.Errorf("got %q, want %q", got, "fallback ok")
	}
}

func TestClient_Vertex_NoFallbackOn401(t *testing.T) {
	// 401 is not retriable — should not try fallback.
	calls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "unauthorized"}`))
	}))
	t.Cleanup(primary.Close)

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write(anthropicResponse("should not reach"))
	}))
	t.Cleanup(fallback.Close)

	client := newVertexClient(t, primary.URL, fallback.URL)
	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no fallback), got %d", calls)
	}
}

func TestClient_Vertex_PrimarySucceedsNoFallback(t *testing.T) {
	fallbackCalled := false
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(anthropicResponse("primary ok"))
	}))
	t.Cleanup(primary.Close)

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.Write(anthropicResponse("fallback"))
	}))
	t.Cleanup(fallback.Close)

	client := newVertexClient(t, primary.URL, fallback.URL)
	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "primary ok" {
		t.Errorf("got %q, want %q", got, "primary ok")
	}
	if fallbackCalled {
		t.Error("fallback should not have been called when primary succeeds")
	}
}

func TestClient_Vertex_AllRegionsFail(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error": "down"}`))
	})
	primary := httptest.NewServer(handler)
	t.Cleanup(primary.Close)
	fb1 := httptest.NewServer(handler)
	t.Cleanup(fb1.Close)
	fb2 := httptest.NewServer(handler)
	t.Cleanup(fb2.Close)

	client := newVertexClient(t, primary.URL, fb1.URL, fb2.URL)
	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err == nil {
		t.Fatal("expected error when all regions fail")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("expected 503 in error, got: %v", err)
	}
}

func TestClient_Vertex_MultipleFallbacks(t *testing.T) {
	// Primary and first fallback fail, second fallback succeeds.
	failHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error": "bad gateway"}`))
	})
	primary := httptest.NewServer(failHandler)
	t.Cleanup(primary.Close)
	fb1 := httptest.NewServer(failHandler)
	t.Cleanup(fb1.Close)
	fb2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(anthropicResponse("third region"))
	}))
	t.Cleanup(fb2.Close)

	client := newVertexClient(t, primary.URL, fb1.URL, fb2.URL)
	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("expected success from third region, got error: %v", err)
	}
	if got != "third region" {
		t.Errorf("got %q, want %q", got, "third region")
	}
}

// ── Shared ────────────────────────────────────────────────────────────────────

func TestClient_Complete_TrailingSlashStripped(t *testing.T) {
	// Endpoint with trailing slash should still hit /chat/completions correctly.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(oaResponse("trimmed"))
	}))
	t.Cleanup(ts.Close)

	cfg := config.LLMProviderConfig{
		Endpoint:       ts.URL + "/v1/", // trailing slash
		APIKey:         "key",
		Model:          "m",
		TimeoutSeconds: 5,
	}
	client := llm.NewClient(cfg)
	got, err := client.Complete(context.Background(), []llm.ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("trailing slash: unexpected error: %v", err)
	}
	if got != "trimmed" {
		t.Errorf("trailing slash: got %q, want trimmed", got)
	}
}
