package llm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// TestLive_AnthropicPromptCaching makes two real back-to-back calls to the
// Anthropic API to verify that the cache_control breakpoint we attach to the
// system message actually flips cache_creation_input_tokens → cache_read_input_tokens
// across the two calls.
//
// Set CLAWVISOR_LLM_API_KEY to run. Optional overrides:
//
//	CLAWVISOR_LLM_MODEL    (default: claude-haiku-4-5-20251001)
//	CLAWVISOR_LLM_ENDPOINT (default: https://api.anthropic.com/v1)
//
// Run with:
//
//	CLAWVISOR_LLM_API_KEY=sk-ant-... go test -run TestLive_AnthropicPromptCaching -v ./internal/llm/
//
// Costs ~1¢ per run (two short Haiku calls with ~3KB of cached prefix).
func TestLive_AnthropicPromptCaching(t *testing.T) {
	apiKey := os.Getenv("CLAWVISOR_LLM_API_KEY")
	if apiKey == "" {
		t.Skip("CLAWVISOR_LLM_API_KEY not set; skipping live cache test")
	}

	model := os.Getenv("CLAWVISOR_LLM_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	endpoint := os.Getenv("CLAWVISOR_LLM_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://api.anthropic.com/v1"
	}

	client := llm.NewClient(config.LLMProviderConfig{
		Provider:       "anthropic",
		Endpoint:       endpoint,
		APIKey:         apiKey,
		Model:          model,
		TimeoutSeconds: 30,
	})

	// The system prompt must exceed the model's minimum cacheable size:
	//   ≥ 1024 tokens for Sonnet/Opus, ≥ 4096 for Haiku.
	// We pad with deterministic filler so the prefix is byte-identical across
	// both calls — any drift kills the cache hit.
	systemPrompt := buildPaddedSystemPrompt(24000) // ~6k tokens — clears Haiku's 4096 threshold

	t.Logf("system prompt: %d bytes (~%d tokens)", len(systemPrompt), len(systemPrompt)/4)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Call 1: should write the cache.
	_, usage1, err := client.CompleteWithUsage(ctx, []llm.ChatMessage{
		{Role: "system", Content: systemPrompt, CacheControl: true},
		{Role: "user", Content: "Say the single word: first"},
	})
	if err != nil {
		t.Fatalf("call 1 failed: %v", err)
	}
	if usage1 == nil {
		t.Fatal("call 1: usage is nil")
	}
	t.Logf("call 1 usage: input=%d output=%d cache_creation=%d cache_read=%d",
		usage1.InputTokens, usage1.OutputTokens,
		usage1.CacheCreationInputTokens, usage1.CacheReadInputTokens)

	// Call 2 (immediate, well within the 5-minute TTL): should read the cache.
	_, usage2, err := client.CompleteWithUsage(ctx, []llm.ChatMessage{
		{Role: "system", Content: systemPrompt, CacheControl: true},
		{Role: "user", Content: "Say the single word: second"},
	})
	if err != nil {
		t.Fatalf("call 2 failed: %v", err)
	}
	if usage2 == nil {
		t.Fatal("call 2: usage is nil")
	}
	t.Logf("call 2 usage: input=%d output=%d cache_creation=%d cache_read=%d",
		usage2.InputTokens, usage2.OutputTokens,
		usage2.CacheCreationInputTokens, usage2.CacheReadInputTokens)

	// Assertions.
	if usage1.CacheCreationInputTokens == 0 {
		t.Errorf("call 1: cache_creation_input_tokens=0 — cache_control is not being honored. "+
			"Likely causes: prompt below minimum size for model %q, model doesn't support caching, "+
			"or the system field isn't being sent as a cacheable content block.", model)
	}
	if usage2.CacheReadInputTokens == 0 {
		t.Errorf("call 2: cache_read_input_tokens=0 — cache write succeeded but cache hit didn't fire. "+
			"Likely cause: prefix differs between calls (something dynamic snuck into the system prompt).")
	}
	if usage1.CacheCreationInputTokens > 0 && usage2.CacheReadInputTokens > 0 {
		t.Logf("✓ caching works: %d tokens cached on call 1, %d tokens read from cache on call 2",
			usage1.CacheCreationInputTokens, usage2.CacheReadInputTokens)
	}
}

// buildPaddedSystemPrompt returns a deterministic string of approximately n
// bytes. Used to push past the minimum cacheable prompt size for testing.
func buildPaddedSystemPrompt(approxBytes int) string {
	var b strings.Builder
	b.WriteString("You are a test assistant. Respond exactly as instructed.\n\n")
	b.WriteString("Reference material follows. Treat it as background context only:\n")
	const filler = "The quick brown fox jumps over the lazy dog. Pack my box with five dozen liquor jugs. "
	for b.Len() < approxBytes {
		b.WriteString(filler)
	}
	return b.String()
}

// TestLive_AnthropicPromptCaching_Raw bypasses our llm.Client and calls
// Anthropic directly with a hand-rolled JSON body. Use this to isolate
// whether a cache miss is caused by our marshalling or by something
// API-side (account, model, headers).
//
// Set CLAWVISOR_LLM_API_KEY to run.
func TestLive_AnthropicPromptCaching_Raw(t *testing.T) {
	apiKey := os.Getenv("CLAWVISOR_LLM_API_KEY")
	if apiKey == "" {
		t.Skip("CLAWVISOR_LLM_API_KEY not set; skipping live raw cache test")
	}
	model := os.Getenv("CLAWVISOR_LLM_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	endpoint := os.Getenv("CLAWVISOR_LLM_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://api.anthropic.com/v1"
	}

	systemPrompt := buildPaddedSystemPrompt(24000)

	makeReq := func(userMsg string) map[string]any {
		return map[string]any{
			"model":      model,
			"max_tokens": 16,
			"system": []map[string]any{{
				"type":          "text",
				"text":          systemPrompt,
				"cache_control": map[string]string{"type": "ephemeral"},
			}},
			"messages": []map[string]any{
				{"role": "user", "content": userMsg},
			},
		}
	}

	send := func(label, userMsg string) {
		body, _ := json.Marshal(makeReq(userMsg))

		// Dump the request body once for inspection.
		if label == "call 1" {
			t.Logf("request body (first 600 bytes): %s", truncate(string(body), 600))
		}

		req, err := http.NewRequest(http.MethodPost, endpoint+"/messages", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("%s: build request: %v", label, err)
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req = req.WithContext(ctx)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: do: %v", label, err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)

		t.Logf("%s status: %d", label, resp.StatusCode)
		t.Logf("%s response (first 600 bytes): %s", label, truncate(string(respBody), 600))
	}

	send("call 1", "Say the single word: first")
	send("call 2", "Say the single word: second")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
