package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// geminiResponse builds a minimal generateContent response with usageMetadata.
func geminiResponse(text string, cachedTokens int) []byte {
	b, _ := json.Marshal(map[string]any{
		"candidates": []map[string]any{
			{
				"content": map[string]any{
					"parts": []map[string]any{{"text": text}},
					"role":  "model",
				},
				"finishReason": "STOP",
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":        cachedTokens + 100,
			"candidatesTokenCount":    20,
			"cachedContentTokenCount": cachedTokens,
			"totalTokenCount":         cachedTokens + 120,
		},
	})
	return b
}

func newGeminiServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, config.LLMProviderConfig) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, config.LLMProviderConfig{
		Provider:       "gemini",
		Endpoint:       ts.URL, // bypass URL construction; client uses Endpoint as-is
		Model:          "gemini-test",
		TimeoutSeconds: 5,
	}
}

func TestClient_Gemini_UncachedPath_InlinesSystemInstruction(t *testing.T) {
	var captured map[string]any
	ts, cfg := newGeminiServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		if r.Header.Get("Authorization") == "" {
			t.Error("expected Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(geminiResponse("hello", 0))
	})
	_ = ts

	client := llm.NewClient(cfg).WithTokenSource(staticToken{})

	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "you are a verifier"},
		{Role: "user", Content: "verify this"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want hello", got)
	}

	// systemInstruction must be present (no cache).
	si, ok := captured["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("expected systemInstruction in body; got %v", captured)
	}
	parts, _ := si["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("expected one part in systemInstruction; got %v", parts)
	}
	if part, _ := parts[0].(map[string]any); part["text"] != "you are a verifier" {
		t.Errorf("system text: %v", part)
	}
	// cachedContent must NOT be present.
	if _, has := captured["cachedContent"]; has {
		t.Error("cachedContent should not be set on the uncached path")
	}
	// generationConfig defaults.
	gc, _ := captured["generationConfig"].(map[string]any)
	tc, _ := gc["thinkingConfig"].(map[string]any)
	if tc["thinkingLevel"] != "MINIMAL" {
		t.Errorf("default thinkingLevel: got %v, want MINIMAL", tc["thinkingLevel"])
	}
}

func TestClient_Gemini_CachedPath_ReferencesCacheAndOmitsSystem(t *testing.T) {
	var captured map[string]any
	_, cfg := newGeminiServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		w.Write(geminiResponse("ok", 5500))
	})

	client := llm.NewClient(cfg).WithTokenSource(staticToken{})
	const cacheName = "projects/p/locations/global/cachedContents/abc123"
	client.AttachGeminiCacheNameFn(func() string { return cacheName })

	_, usage, err := client.CompleteWithUsage(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "you are a verifier"},
		{Role: "user", Content: "verify"},
	})
	if err != nil {
		t.Fatalf("CompleteWithUsage: %v", err)
	}
	if got := captured["cachedContent"]; got != cacheName {
		t.Errorf("cachedContent: got %v, want %s", got, cacheName)
	}
	if _, has := captured["systemInstruction"]; has {
		t.Error("systemInstruction must be omitted when cachedContent is set (mutually exclusive on the API)")
	}
	if usage.CacheReadInputTokens != 5500 {
		t.Errorf("CacheReadInputTokens: got %d, want 5500", usage.CacheReadInputTokens)
	}
}

func TestClient_Gemini_CachedPath_FallsThroughWhenCacheNameEmpty(t *testing.T) {
	var captured map[string]any
	_, cfg := newGeminiServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		w.Write(geminiResponse("ok", 0))
	})

	client := llm.NewClient(cfg).WithTokenSource(staticToken{})
	// Cache function returns "" — should fall through to inline systemInstruction.
	client.AttachGeminiCacheNameFn(func() string { return "" })

	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "system text"},
		{Role: "user", Content: "user text"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, has := captured["cachedContent"]; has {
		t.Error("cachedContent should not be sent when cache function returns empty")
	}
	if _, has := captured["systemInstruction"]; !has {
		t.Error("systemInstruction must be present when cache is unavailable")
	}
}

func TestClient_Gemini_ConvertsAssistantToModelRole(t *testing.T) {
	var captured map[string]any
	_, cfg := newGeminiServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		w.Write(geminiResponse("ok", 0))
	})
	client := llm.NewClient(cfg).WithTokenSource(staticToken{})

	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u"},
		{Role: "assistant", Content: "a"},
		{Role: "user", Content: "u2"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	contents, _ := captured["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("expected 3 conversation turns (system extracted); got %d", len(contents))
	}
	roles := []string{}
	for _, c := range contents {
		if m, _ := c.(map[string]any); m != nil {
			if r, _ := m["role"].(string); r != "" {
				roles = append(roles, r)
			}
		}
	}
	if strings.Join(roles, ",") != "user,model,user" {
		t.Errorf("roles: got %v, want [user model user]", roles)
	}
}

func TestClient_Gemini_NewClient_BuildsEndpointFromProjectAndRegion(t *testing.T) {
	cfg := config.LLMProviderConfig{
		Provider: "gemini",
		Project:  "my-project",
		Region:   "us-central1",
		Model:    "gemini-3.1-flash-lite-preview",
	}
	c := llm.NewClient(cfg)
	got := c.Endpoint()
	want := "https://us-central1-aiplatform.googleapis.com/v1/projects/my-project/locations/us-central1/publishers/google/models/gemini-3.1-flash-lite-preview:generateContent"
	if got != want {
		t.Errorf("Endpoint:\n  got:  %s\n  want: %s", got, want)
	}
}

func TestClient_Gemini_NewClient_GlobalRegionUsesUnprefixedHost(t *testing.T) {
	cfg := config.LLMProviderConfig{
		Provider: "gemini",
		Project:  "my-project",
		Region:   "global",
		Model:    "gemini-3.1-flash-lite-preview",
	}
	c := llm.NewClient(cfg)
	got := c.Endpoint()
	want := "https://aiplatform.googleapis.com/v1/projects/my-project/locations/global/publishers/google/models/gemini-3.1-flash-lite-preview:generateContent"
	if got != want {
		t.Errorf("global endpoint:\n  got:  %s\n  want: %s", got, want)
	}
}
