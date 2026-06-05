package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestProviderProtocolReplay_AnthropicContinuationSpliceFixture(t *testing.T) {
	spliced, err := spliceStreamingContinuationBody(conversation.ProviderAnthropic, conversation.StreamingRewriteResult{
		StreamFormat:              "anthropic_messages",
		NextAnthropicContentIndex: 2,
	}, "text/event-stream", readHandlerProviderReplayFixture(t, "anthropic/continuation_stream.sse"))
	if err != nil {
		t.Fatalf("spliceStreamingContinuationBody: %v", err)
	}
	out := string(spliced)
	if strings.Contains(out, "event: message_start") {
		t.Fatalf("spliced Anthropic continuation must not duplicate message_start:\n%s", out)
	}
	if got := strings.Count(out, "event: message_stop"); got != 1 {
		t.Fatalf("message_stop count=%d, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, `"index":2`) {
		t.Fatalf("spliced Anthropic continuation did not offset content index to 2:\n%s", out)
	}
}

func TestProviderProtocolReplay_OpenAIResponsesContinuationSpliceFixture(t *testing.T) {
	spliced, err := spliceStreamingContinuationBody(conversation.ProviderOpenAI, conversation.StreamingRewriteResult{
		StreamID:              "resp_original",
		StreamFormat:          "openai_responses",
		NextOpenAIOutputIndex: 1,
	}, "text/event-stream", readHandlerProviderReplayFixture(t, "openai_responses/continuation_stream.sse"))
	if err != nil {
		t.Fatalf("spliceStreamingContinuationBody: %v", err)
	}
	out := string(spliced)
	if strings.Contains(out, "event: response.created") {
		t.Fatalf("spliced Responses continuation must not duplicate response.created:\n%s", out)
	}
	if got := strings.Count(out, "event: response.completed"); got != 1 {
		t.Fatalf("response.completed count=%d, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, `"output_index":1`) {
		t.Fatalf("spliced Responses continuation did not offset output_index to 1:\n%s", out)
	}
	completed := handlerReplayEventData(t, out, "response.completed")
	var payload struct {
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(completed), &payload); err != nil {
		t.Fatalf("response.completed payload malformed: %v\n%s", err, completed)
	}
	if payload.Response.ID != "resp_original" {
		t.Fatalf("response.completed id=%q, want resp_original:\n%s", payload.Response.ID, out)
	}
}

func readHandlerProviderReplayFixture(t *testing.T, rel string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "runtime", "llmproxy", "testdata", "provider_replay", filepath.FromSlash(rel))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	return body
}

func handlerReplayEventData(t *testing.T, stream, event string) string {
	t.Helper()
	for _, block := range strings.Split(strings.ReplaceAll(stream, "\r\n", "\n"), "\n\n") {
		if !strings.Contains(block, "event: "+event) {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				return strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}
	t.Fatalf("event %q not found in stream:\n%s", event, stream)
	return ""
}
