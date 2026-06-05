package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
)

const providerReplayPlaceholder = "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

func TestProviderProtocolReplay_TextOnlyStreams(t *testing.T) {
	tests := []struct {
		name     string
		route    string
		provider string
		fixture  string
		wantText string
	}{
		{"anthropic", "/v1/messages", "anthropic", "anthropic/text_only_stream.sse", "Hello from Anthropic"},
		{"openai chat", "/v1/chat/completions", "openai_chat", "openai_chat/text_only_stream.sse", "Hello from chat"},
		{"openai responses", "/v1/responses", "openai_responses", "openai_responses/text_only_stream.sse", "Hello from responses"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, out, err := runProviderReplayStream(t, tt.route, tt.fixture, replayPostprocessConfig(t, false))
			if err != nil {
				t.Fatalf("PostprocessStream: %v", err)
			}
			if result.Rewritten {
				t.Fatalf("text-only stream should not rewrite: %+v", result)
			}
			if len(result.Decisions) != 0 {
				t.Fatalf("text-only stream produced decisions: %+v", result.Decisions)
			}
			if !strings.Contains(out, tt.wantText) {
				t.Fatalf("text-only stream lost text %q:\n%s", tt.wantText, out)
			}
			assertProviderReplayProtocol(t, tt.provider, out)
		})
	}
}

func TestProviderProtocolReplay_ToolRewriteStreams(t *testing.T) {
	tests := []struct {
		name     string
		route    string
		provider string
		fixture  string
	}{
		{"anthropic", "/v1/messages", "anthropic", "anthropic/tool_rewrite_stream.sse"},
		{"openai chat", "/v1/chat/completions", "openai_chat", "openai_chat/tool_rewrite_stream.sse"},
		{"openai responses", "/v1/responses", "openai_responses", "openai_responses/function_call_rewrite_stream.sse"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, out, err := runProviderReplayStream(t, tt.route, tt.fixture, replayPostprocessConfig(t, true))
			if err != nil {
				t.Fatalf("PostprocessStream: %v", err)
			}
			if !result.Rewritten {
				t.Fatalf("expected tool stream rewrite, got %+v\n%s", result, out)
			}
			if len(result.Decisions) != 1 {
				t.Fatalf("expected one inspected tool call, got %+v", result.Decisions)
			}
			if !strings.Contains(out, "https://proxy.example/api/proxy/repos/x/y/issues") {
				t.Fatalf("rewritten stream missing proxy URL:\n%s", out)
			}
			assertProviderReplayProtocol(t, tt.provider, out)
		})
	}
}

func TestProviderProtocolReplay_BlockedToolStreams(t *testing.T) {
	tests := []struct {
		name     string
		route    string
		provider string
		fixture  string
	}{
		{"anthropic", "/v1/messages", "anthropic", "anthropic/blocked_tool_stream.sse"},
		{"openai chat", "/v1/chat/completions", "openai_chat", "openai_chat/blocked_tool_stream.sse"},
		{"openai responses custom tool", "/v1/responses", "openai_responses", "openai_responses/custom_tool_call_blocked_stream.sse"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, out, err := runProviderReplayStream(t, tt.route, tt.fixture, replayPostprocessConfig(t, false))
			if err != nil {
				t.Fatalf("PostprocessStream: %v", err)
			}
			if !result.Rewritten {
				t.Fatalf("expected blocked tool stream rewrite, got %+v\n%s", result, out)
			}
			if len(result.Decisions) != 1 {
				t.Fatalf("expected one blocked tool decision, got %+v", result.Decisions)
			}
			if !strings.Contains(out, "Tool use was blocked by the Clawvisor proxy") {
				t.Fatalf("blocked stream missing provider-shaped refusal:\n%s", out)
			}
			if strings.Contains(out, providerReplayPlaceholder) {
				t.Fatalf("blocked stream leaked placeholder:\n%s", out)
			}
			if strings.Contains(out, `"custom_tool_call"`) {
				t.Fatalf("blocked Responses custom tool leaked original item:\n%s", out)
			}
			assertProviderReplayProtocol(t, tt.provider, out)
		})
	}
}

func TestProviderProtocolReplay_MalformedStreamsPassThroughWithoutAbort(t *testing.T) {
	tests := []struct {
		name    string
		route   string
		fixture string
	}{
		{"anthropic", "/v1/messages", "anthropic/malformed_partial_stream.sse"},
		{"openai chat", "/v1/chat/completions", "openai_chat/malformed_partial_stream.sse"},
		{"openai responses", "/v1/responses", "openai_responses/malformed_partial_stream.sse"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, out, err := runProviderReplayStream(t, tt.route, tt.fixture, replayPostprocessConfig(t, true))
			if err != nil {
				t.Fatalf("malformed stream should pass through without aborting: %v", err)
			}
			if result.Rewritten {
				t.Fatalf("malformed stream should not be rewritten: %+v", result)
			}
			if out == "" {
				t.Fatal("malformed stream should still deliver passthrough bytes")
			}
		})
	}
}

func TestProviderProtocolReplay_OpenAIResponsesSyntheticKeepsCreatedWhenOriginalMissing(t *testing.T) {
	body, err := openAIResponsesSyntheticForExistingStream([]byte(strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_synthetic","status":"in_progress"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_synthetic","status":"completed"}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")), "resp_original", false)
	if err != nil {
		t.Fatalf("openAIResponsesSyntheticForExistingStream: %v", err)
	}
	out := string(body)
	if !strings.Contains(out, "event: response.created") {
		t.Fatalf("synthetic stream without prior response.created must keep creation event:\n%s", out)
	}
	if !strings.Contains(out, "resp_synthetic") {
		t.Fatalf("synthetic stream without prior response.created should remain unchanged:\n%s", out)
	}
}

func runProviderReplayStream(t *testing.T, route, fixture string, cfg PostprocessConfig) (PostprocessResult, string, error) {
	t.Helper()
	req := httptest.NewRequest("POST", route, nil)
	var output bytes.Buffer
	result, err := PostprocessStream(context.Background(), req, bytes.NewReader(readProviderReplayFixture(t, fixture)), &output, "text/event-stream", cfg)
	return result, output.String(), err
}

func replayPostprocessConfig(t *testing.T, allowRewrite bool) PostprocessConfig {
	t.Helper()
	st, userID, agentID := seedPostprocessStore(t, providerReplayPlaceholder)
	cfg := PostprocessConfig{
		Inspector:   inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
	}
	if allowRewrite {
		cfg.RewriteOpts = inspector.DefaultRewriteOpts("https://proxy.example/api/proxy")
		cfg.CallerNonces = NewMemoryCallerNonceCache(time.Minute)
	}
	return cfg
}

func readProviderReplayFixture(t *testing.T, rel string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "provider_replay", filepath.FromSlash(rel))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	return body
}

type replaySSEEvent struct {
	Event string
	Data  string
}

func parseProviderReplaySSE(t *testing.T, stream string) []replaySSEEvent {
	t.Helper()
	blocks := strings.Split(strings.ReplaceAll(stream, "\r\n", "\n"), "\n\n")
	events := make([]replaySSEEvent, 0, len(blocks))
	for _, block := range blocks {
		block = strings.TrimSuffix(block, "\n")
		if strings.TrimSpace(block) == "" {
			continue
		}
		var ev replaySSEEvent
		var dataLines []string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSuffix(line, "\r")
			switch {
			case strings.HasPrefix(line, "event:"):
				ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data := strings.TrimPrefix(line, "data:")
				if strings.HasPrefix(data, " ") {
					data = data[1:]
				}
				dataLines = append(dataLines, data)
			}
		}
		if len(dataLines) == 0 {
			continue
		}
		ev.Data = strings.Join(dataLines, "\n")
		events = append(events, ev)
	}
	return events
}

func assertProviderReplayProtocol(t *testing.T, provider, stream string) {
	t.Helper()
	switch provider {
	case "anthropic":
		assertAnthropicReplayProtocol(t, stream)
	case "openai_chat":
		assertOpenAIChatReplayProtocol(t, stream)
	case "openai_responses":
		assertOpenAIResponsesReplayProtocol(t, stream)
	default:
		t.Fatalf("unknown provider %q", provider)
	}
}

func assertAnthropicReplayProtocol(t *testing.T, stream string) {
	t.Helper()
	events := parseProviderReplaySSE(t, stream)
	messageStart, messageStop := 0, 0
	started := map[int]bool{}
	stopped := map[int]bool{}
	for _, ev := range events {
		switch ev.Event {
		case "message_start":
			messageStart++
		case "message_stop":
			messageStop++
		case "content_block_start", "content_block_delta", "content_block_stop":
			idx := replayEventIndex(t, ev)
			if idx < 0 {
				t.Fatalf("negative Anthropic content index in %s: %s", ev.Event, ev.Data)
			}
			switch ev.Event {
			case "content_block_start":
				if started[idx] {
					t.Fatalf("duplicate Anthropic content_block_start index %d:\n%s", idx, stream)
				}
				started[idx] = true
			case "content_block_delta":
				if !started[idx] {
					t.Fatalf("Anthropic delta before start for index %d:\n%s", idx, stream)
				}
			case "content_block_stop":
				if !started[idx] {
					t.Fatalf("Anthropic stop before start for index %d:\n%s", idx, stream)
				}
				stopped[idx] = true
			}
		}
	}
	if messageStart != 1 {
		t.Fatalf("Anthropic stream has %d message_start events, want 1:\n%s", messageStart, stream)
	}
	if messageStop != 1 {
		t.Fatalf("Anthropic stream has %d message_stop events, want 1:\n%s", messageStop, stream)
	}
	for idx := range started {
		if !stopped[idx] {
			t.Fatalf("Anthropic content block index %d was not stopped:\n%s", idx, stream)
		}
	}
}

func assertOpenAIChatReplayProtocol(t *testing.T, stream string) {
	t.Helper()
	events := parseProviderReplaySSE(t, stream)
	done, terminal := 0, false
	for _, ev := range events {
		if ev.Data == "[DONE]" {
			done++
			continue
		}
		var payload struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
				Delta        struct {
					ToolCalls []struct {
						Index int `json:"index"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			t.Fatalf("malformed OpenAI Chat SSE data: %v\n%s", err, ev.Data)
		}
		for _, choice := range payload.Choices {
			if choice.FinishReason != nil && (*choice.FinishReason == "stop" || *choice.FinishReason == "tool_calls") {
				terminal = true
			}
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Index < 0 {
					t.Fatalf("negative OpenAI Chat tool index: %s", ev.Data)
				}
			}
		}
	}
	if done != 1 {
		t.Fatalf("OpenAI Chat stream has %d [DONE] sentinels, want 1:\n%s", done, stream)
	}
	if !terminal {
		t.Fatalf("OpenAI Chat stream missing terminal stop/tool_calls chunk:\n%s", stream)
	}
}

func assertOpenAIResponsesReplayProtocol(t *testing.T, stream string) {
	t.Helper()
	events := parseProviderReplaySSE(t, stream)
	created, completed, done := 0, 0, 0
	for _, ev := range events {
		if ev.Data == "[DONE]" {
			done++
			continue
		}
		if ev.Event == "response.created" {
			created++
		}
		if ev.Event == "response.completed" {
			completed++
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			t.Fatalf("malformed OpenAI Responses SSE data in %s: %v\n%s", ev.Event, err, ev.Data)
		}
		if idx, ok, valid := replayNumericIndex(payload["output_index"]); ok {
			if !valid {
				t.Fatalf("fractional OpenAI Responses output_index in %s: %s", ev.Event, ev.Data)
			}
			if idx < 0 {
				t.Fatalf("negative OpenAI Responses output_index in %s: %s", ev.Event, ev.Data)
			}
		}
	}
	if created > 1 {
		t.Fatalf("OpenAI Responses stream has %d response.created events:\n%s", created, stream)
	}
	if completed != 1 {
		t.Fatalf("OpenAI Responses stream has %d response.completed events, want 1:\n%s", completed, stream)
	}
	if done != 1 {
		t.Fatalf("OpenAI Responses stream has %d [DONE] sentinels, want 1:\n%s", done, stream)
	}
}

func replayEventIndex(t *testing.T, ev replaySSEEvent) int {
	t.Helper()
	var payload struct {
		Index *int `json:"index"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("malformed SSE event data in %s: %v\n%s", ev.Event, err, ev.Data)
	}
	if payload.Index == nil {
		t.Fatalf("SSE event %s missing index: %s", ev.Event, ev.Data)
	}
	return *payload.Index
}

func replayNumericIndex(v any) (int, bool, bool) {
	switch n := v.(type) {
	case float64:
		if n != float64(int(n)) {
			return 0, true, false
		}
		return int(n), true, true
	case int:
		return n, true, true
	default:
		return 0, false, true
	}
}
