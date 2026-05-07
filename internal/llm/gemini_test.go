package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestClient_Gemini_404OnCachedRequest_RetriesWithInlineSystem(t *testing.T) {
	// First request references cachedContent and gets a 404 (cache TTL
	// elapsed server-side or refresh is failing). Client should retry once
	// with systemInstruction inlined, succeeding the second attempt.
	var bodies []map[string]any
	_, cfg := newGeminiServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"code":404,"message":"cached content gone"}}`))
			return
		}
		w.Write(geminiResponse("recovered", 0))
	})

	client := llm.NewClient(cfg).WithTokenSource(staticToken{})
	client.AttachGeminiCacheNameFn(func() string {
		return "projects/p/locations/global/cachedContents/abc"
	})

	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "system text"},
		{Role: "user", Content: "user text"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "recovered" {
		t.Errorf("got %q, want recovered", got)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 requests (cached attempt + inline retry), got %d", len(bodies))
	}
	// First request must have used cachedContent.
	if _, has := bodies[0]["cachedContent"]; !has {
		t.Error("first request should have set cachedContent")
	}
	if _, has := bodies[0]["systemInstruction"]; has {
		t.Error("first request should NOT have set systemInstruction")
	}
	// Second request must have inlined systemInstruction and dropped cachedContent.
	if _, has := bodies[1]["cachedContent"]; has {
		t.Error("retry should NOT have set cachedContent")
	}
	if _, has := bodies[1]["systemInstruction"]; !has {
		t.Error("retry should have inlined systemInstruction")
	}
}

func TestClient_Gemini_404Uncached_NoRetry(t *testing.T) {
	// 404 on a request that was already uncached should NOT retry — there's
	// nothing to fall back to. The error surfaces to the caller.
	calls := 0
	_, cfg := newGeminiServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":404,"message":"model not found"}}`))
	})
	client := llm.NewClient(cfg).WithTokenSource(staticToken{})
	// No AttachGeminiCacheNameFn — uncached path.

	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
	})
	if err == nil {
		t.Fatal("expected 404 error to surface")
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call (no retry), got %d", calls)
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

func TestClient_Hedge_FastPrimary_NoHedgeFires(t *testing.T) {
	// When the primary returns within hedgeDelay, no hedge is fired and
	// the server sees exactly one request.
	var calls int32
	_, cfg := newGeminiServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(geminiResponse("primary", 0))
	})
	client := llm.NewClient(cfg).
		WithTokenSource(staticToken{}).
		WithHedgeDelay(500 * time.Millisecond)

	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "primary" {
		t.Errorf("got %q, want primary", got)
	}
	// Brief sleep so any erroneously-fired hedge would have time to land.
	time.Sleep(50 * time.Millisecond)
	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Errorf("expected 1 server call (no hedge), got %d", c)
	}
}

func TestClient_Hedge_SlowPrimary_HedgeWins(t *testing.T) {
	// Primary stalls past hedgeDelay → hedge fires and returns first.
	var calls int32
	_, cfg := newGeminiServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// Primary: stall well past hedgeDelay so the hedge wins.
			// httptest doesn't reliably propagate client-side cancellation
			// to r.Context(), so cap with a short fallback timer.
			select {
			case <-r.Context().Done():
			case <-time.After(300 * time.Millisecond):
			}
			return
		}
		// Hedge: return immediately.
		w.Header().Set("Content-Type", "application/json")
		w.Write(geminiResponse("hedge", 0))
	})
	client := llm.NewClient(cfg).
		WithTokenSource(staticToken{}).
		WithHedgeDelay(50 * time.Millisecond)

	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "hedge" {
		t.Errorf("got %q, want hedge", got)
	}
	if c := atomic.LoadInt32(&calls); c != 2 {
		t.Errorf("expected 2 server calls, got %d", c)
	}
}

func TestClient_Hedge_PrimaryFailsFast_NoHedge(t *testing.T) {
	// Primary returns an error before hedgeDelay elapses. Hedge should NOT
	// fire — there's no point hedging against a deterministic failure.
	var calls int32
	_, cfg := newGeminiServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	})
	client := llm.NewClient(cfg).
		WithTokenSource(staticToken{}).
		WithHedgeDelay(500 * time.Millisecond)

	_, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
	})
	if err == nil {
		t.Fatal("expected error from 500 response")
	}
	time.Sleep(50 * time.Millisecond)
	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Errorf("expected 1 server call (no hedge after fast failure), got %d", c)
	}
}

func TestClient_Hedge_BothFail_ReturnsError(t *testing.T) {
	// Primary stalls past hedgeDelay then fails; hedge also fails.
	// Caller should see an error (not a hang).
	var calls int32
	_, cfg := newGeminiServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// Stall just long enough to let hedge fire, then return 500.
			time.Sleep(100 * time.Millisecond)
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	})
	client := llm.NewClient(cfg).
		WithTokenSource(staticToken{}).
		WithHedgeDelay(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Complete(ctx, []llm.ChatMessage{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
	})
	if err == nil {
		t.Fatal("expected error when both primary and hedge fail")
	}
	if c := atomic.LoadInt32(&calls); c != 2 {
		t.Errorf("expected 2 server calls, got %d", c)
	}
}

func TestNewGeminiCacheManager_RejectsNegativeTTL(t *testing.T) {
	// Regression: negative TTL would propagate to time.NewTicker in the
	// refresh goroutine and panic the process. Constructor must reject it.
	_, err := llm.NewGeminiCacheManager(llm.GeminiCacheManagerConfig{
		Project:      "p",
		Region:       "global",
		Model:        "gemini-test",
		SystemPrompt: "sys",
		TTL:          -1 * time.Second,
		TokenSource:  staticToken{},
	})
	if err == nil {
		t.Fatal("expected error for negative TTL, got nil")
	}
	if !strings.Contains(err.Error(), "TTL") {
		t.Errorf("error should mention TTL, got: %v", err)
	}
}

func TestClient_Gemini_LargeSuccessResponse_ReadInFull(t *testing.T) {
	// Regression: an earlier version capped resp body reads at 64KB even on
	// success, so long extraction outputs (8192-token cap, plus per-part
	// thoughtSignature blobs) silently truncated mid-JSON and the decoder
	// returned a parse error on otherwise valid responses. The success path
	// must read the entire body.
	const partLen = 200 * 1024 // 200KB of text — well past the old 64KB cap
	largeText := strings.Repeat("x", partLen)
	_, cfg := newGeminiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(geminiResponse(largeText, 0))
	})
	client := llm.NewClient(cfg).WithTokenSource(staticToken{})

	got, err := client.Complete(context.Background(), []llm.ChatMessage{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(got) != partLen {
		t.Errorf("text length: got %d bytes, want %d (response was truncated)", len(got), partLen)
	}
}
