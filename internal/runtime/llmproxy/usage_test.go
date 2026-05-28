package llmproxy

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestExtractUsage_AnthropicJSON(t *testing.T) {
	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant",
		"model":"claude-sonnet-4-7",
		"content":[{"type":"text","text":"hi"}],
		"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":20,"cache_creation_input_tokens":10}
	}`)
	got := ExtractUsage(conversation.ProviderAnthropic, "application/json", body, "")
	if !got.Found {
		t.Fatal("expected Found=true")
	}
	if got.Model != "claude-sonnet-4-7" {
		t.Errorf("Model = %q, want claude-sonnet-4-7", got.Model)
	}
	if got.Usage.InputTokens != 100 || got.Usage.OutputTokens != 50 {
		t.Errorf("input/output = %d/%d, want 100/50", got.Usage.InputTokens, got.Usage.OutputTokens)
	}
	if got.Usage.CacheReadTokens != 20 || got.Usage.CacheWriteTokens != 10 {
		t.Errorf("cache read/write = %d/%d, want 20/10", got.Usage.CacheReadTokens, got.Usage.CacheWriteTokens)
	}
}

func TestExtractUsage_AnthropicJSON_PerTTLCacheCreation(t *testing.T) {
	body := []byte(`{
		"id":"msg_2","type":"message","role":"assistant",
		"model":"claude-opus-4-7",
		"content":[{"type":"text","text":"hi"}],
		"usage":{
			"input_tokens":100,"output_tokens":50,
			"cache_read_input_tokens":20,
			"cache_creation_input_tokens":30,
			"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10}
		}
	}`)
	got := ExtractUsage(conversation.ProviderAnthropic, "application/json", body, "")
	if !got.Found {
		t.Fatal("expected Found=true")
	}
	// When the per-TTL breakdown is present it takes precedence
	// over the legacy scalar.
	if got.Usage.CacheWriteTokens != 20 {
		t.Errorf("CacheWriteTokens (5m) = %d, want 20", got.Usage.CacheWriteTokens)
	}
	if got.Usage.CacheWrite1hTokens != 10 {
		t.Errorf("CacheWrite1hTokens = %d, want 10", got.Usage.CacheWrite1hTokens)
	}
}

func TestExtractUsage_AnthropicJSON_LegacyCacheCreationFallback(t *testing.T) {
	// Older responses only carry the scalar — falls into the 5m bucket.
	body := []byte(`{"model":"claude-opus-4-7","usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":42}}`)
	got := ExtractUsage(conversation.ProviderAnthropic, "application/json", body, "")
	if got.Usage.CacheWriteTokens != 42 || got.Usage.CacheWrite1hTokens != 0 {
		t.Errorf("legacy fallback wrong: 5m=%d 1h=%d", got.Usage.CacheWriteTokens, got.Usage.CacheWrite1hTokens)
	}
}

func TestExtractUsage_AnthropicSSE(t *testing.T) {
	body := []byte("" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1","model":"claude-opus-4-7","usage":{"input_tokens":200,"cache_read_input_tokens":50,"cache_creation_input_tokens":25,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":150}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n",
	)
	got := ExtractUsage(conversation.ProviderAnthropic, "text/event-stream", body, "claude-opus-4-7")
	if !got.Found {
		t.Fatal("expected Found=true")
	}
	if got.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q, want claude-opus-4-7", got.Model)
	}
	if got.Usage.InputTokens != 200 || got.Usage.OutputTokens != 150 {
		t.Errorf("input/output = %d/%d, want 200/150", got.Usage.InputTokens, got.Usage.OutputTokens)
	}
	if got.Usage.CacheReadTokens != 50 || got.Usage.CacheWriteTokens != 25 {
		t.Errorf("cache read/write = %d/%d, want 50/25", got.Usage.CacheReadTokens, got.Usage.CacheWriteTokens)
	}
}

func TestExtractUsage_OpenAIJSON(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-1","model":"gpt-4o",
		"choices":[{"message":{"role":"assistant","content":"hi"}}],
		"usage":{"prompt_tokens":300,"completion_tokens":80,"prompt_tokens_details":{"cached_tokens":120}}
	}`)
	got := ExtractUsage(conversation.ProviderOpenAI, "application/json", body, "")
	if !got.Found {
		t.Fatal("expected Found=true")
	}
	// prompt_tokens includes cached; we split: 300 - 120 = 180 uncached input.
	if got.Usage.InputTokens != 180 {
		t.Errorf("InputTokens = %d, want 180 (300 - 120 cached)", got.Usage.InputTokens)
	}
	if got.Usage.CacheReadTokens != 120 {
		t.Errorf("CacheReadTokens = %d, want 120", got.Usage.CacheReadTokens)
	}
	if got.Usage.OutputTokens != 80 {
		t.Errorf("OutputTokens = %d, want 80", got.Usage.OutputTokens)
	}
}

func TestExtractUsage_DetectsSSEFromCommentLine(t *testing.T) {
	// SSE intermediaries (CDN proxies, etc.) often send `:` comment
	// lines as a keep-alive heartbeat before the first event. The
	// body sniff has to recognize that as SSE — otherwise a stream
	// that opens with `: ping\n\n` would fall through to the JSON
	// parser and silently drop the usage row when Content-Type was
	// also absent.
	body := []byte("" +
		`: keep-alive heartbeat` + "\n\n" +
		`event: response.completed` + "\n" +
		`data: {"type":"response.completed","response":{"id":"r1","model":"gpt-5.5","usage":{"input_tokens":42,"output_tokens":7}}}` + "\n\n",
	)
	got := ExtractUsage(conversation.ProviderOpenAI, "", body, "gpt-5.5")
	if !got.Found {
		t.Fatal("body opening with `:` comment must still be detected as SSE")
	}
	if got.Usage.InputTokens != 42 || got.Usage.OutputTokens != 7 {
		t.Errorf("got in/out %d/%d, want 42/7", got.Usage.InputTokens, got.Usage.OutputTokens)
	}
}

func TestExtractUsage_DetectsSSEFromBodyWhenContentTypeMissing(t *testing.T) {
	// Regression: Codex traffic was arriving with the Content-Type
	// header empty by the time our extractor saw it, so the SSE
	// branch never fired and Found stayed false despite usage being
	// present in the body. We now detect SSE from the body shape.
	body := []byte("" +
		`event: response.completed` + "\n" +
		`data: {"type":"response.completed","response":{"id":"r1","model":"gpt-5.5","usage":{"input_tokens":300,"output_tokens":40}}}` + "\n\n",
	)
	got := ExtractUsage(conversation.ProviderOpenAI, "", body, "gpt-5.5")
	if !got.Found {
		t.Fatal("empty content-type with SSE body should still extract")
	}
	if got.Usage.OutputTokens != 40 || got.Usage.InputTokens != 300 {
		t.Errorf("in=%d out=%d, want 300/40", got.Usage.InputTokens, got.Usage.OutputTokens)
	}
	// Negative case: a JSON body must still go through the JSON
	// branch — bodyLooksLikeSSE only flips true for actual SSE
	// preludes, not for a `{` opener.
	jsonBody := []byte(`{"model":"gpt-5.5","usage":{"input_tokens":50,"output_tokens":10}}`)
	got = ExtractUsage(conversation.ProviderOpenAI, "", jsonBody, "")
	if !got.Found {
		t.Fatal("empty content-type with JSON body should still extract via JSON path")
	}
}

func TestExtractUsage_OpenAIResponsesJSON(t *testing.T) {
	// Responses API uses input_tokens / output_tokens, not the Chat
	// Completions fields. Before the fix this returned Found=false.
	body := []byte(`{
		"id":"resp_1","model":"gpt-4o",
		"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],
		"usage":{"input_tokens":300,"output_tokens":80,"input_tokens_details":{"cached_tokens":120}}
	}`)
	got := ExtractUsage(conversation.ProviderOpenAI, "application/json", body, "")
	if !got.Found {
		t.Fatal("expected Found=true for Responses-API shape")
	}
	if got.Usage.InputTokens != 180 {
		t.Errorf("InputTokens = %d, want 180 (300 - 120 cached)", got.Usage.InputTokens)
	}
	if got.Usage.OutputTokens != 80 {
		t.Errorf("OutputTokens = %d, want 80", got.Usage.OutputTokens)
	}
	if got.Usage.CacheReadTokens != 120 {
		t.Errorf("CacheReadTokens = %d, want 120", got.Usage.CacheReadTokens)
	}
}

func TestExtractUsage_OpenAIResponsesSSE(t *testing.T) {
	body := []byte("" +
		`data: {"type":"response.in_progress","response":{"id":"r1","model":"gpt-4o"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r1","model":"gpt-4o","usage":{"input_tokens":50,"output_tokens":10}}}` + "\n\n",
	)
	got := ExtractUsage(conversation.ProviderOpenAI, "text/event-stream", body, "gpt-4o")
	if !got.Found {
		t.Fatal("expected Found=true for Responses-API SSE")
	}
	if got.Usage.InputTokens != 50 || got.Usage.OutputTokens != 10 {
		t.Errorf("got input/output %d/%d, want 50/10", got.Usage.InputTokens, got.Usage.OutputTokens)
	}
}

func TestExtractUsage_OpenAISSE_NoUsage(t *testing.T) {
	// OpenAI SSE without include_usage=true never emits usage.
	body := []byte("" +
		`data: {"id":"c1","model":"gpt-4o","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: [DONE]` + "\n\n",
	)
	got := ExtractUsage(conversation.ProviderOpenAI, "text/event-stream", body, "gpt-4o")
	if got.Found {
		t.Errorf("expected Found=false when no usage in stream")
	}
}

func TestExtractUsage_OpenAISSE_WithUsage(t *testing.T) {
	body := []byte("" +
		`data: {"id":"c1","model":"gpt-4o","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":10}}` + "\n\n" +
		`data: [DONE]` + "\n\n",
	)
	got := ExtractUsage(conversation.ProviderOpenAI, "text/event-stream", body, "gpt-4o")
	if !got.Found {
		t.Fatal("expected Found=true")
	}
	if got.Usage.InputTokens != 50 || got.Usage.OutputTokens != 10 {
		t.Errorf("got input/output %d/%d, want 50/10", got.Usage.InputTokens, got.Usage.OutputTokens)
	}
}

func TestExtractUsage_EmptyBody(t *testing.T) {
	got := ExtractUsage(conversation.ProviderAnthropic, "application/json", nil, "claude-opus-4-7")
	if got.Found {
		t.Errorf("empty body should report Found=false")
	}
}
