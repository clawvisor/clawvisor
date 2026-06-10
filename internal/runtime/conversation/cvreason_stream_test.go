package conversation

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestAnthropicStreamRewriteExtractsCvReason verifies the streaming
// Anthropic rewriter extracts the agent-supplied cvreason from a tool_use
// input, populates ToolUse.CvReason, and strips it from the buffered
// Input bytes so the client never sees it.
func TestAnthropicStreamRewriteExtractsCvReason(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"src/auth.go\",\"cvreason\":\"locating login handler\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var output bytes.Buffer
	var delivered []ToolUse
	res, err := (AnthropicResponseRewriter{}).StreamRewrite(
		context.Background(),
		strings.NewReader(input),
		&output,
		func(tu ToolUse) { delivered = append(delivered, tu) },
	)
	if err != nil {
		t.Fatalf("StreamRewrite: %v", err)
	}
	if len(res.ToolUses) != 1 {
		t.Fatalf("ToolUses len = %d, want 1", len(res.ToolUses))
	}
	if len(delivered) != 1 {
		t.Fatalf("onToolUse delivered = %d, want 1", len(delivered))
	}
	tu := res.ToolUses[0]
	if tu.CvReason != "locating login handler" {
		t.Errorf("CvReason = %q, want %q", tu.CvReason, "locating login handler")
	}
	if strings.Contains(string(tu.Input), "cvreason") {
		t.Errorf("Input retained cvreason: %s", tu.Input)
	}
	if !strings.Contains(string(tu.Input), "src/auth.go") {
		t.Errorf("Input lost real params: %s", tu.Input)
	}
	if delivered[0].CvReason != tu.CvReason {
		t.Errorf("onToolUse CvReason = %q, want %q", delivered[0].CvReason, tu.CvReason)
	}
}

// TestOpenAIChatCompletionsSSEStripsCvReasonWithoutPolicyVerdict verifies
// that the chat-completions SSE rewriter's cvreason-only path produces
// stripped output bytes and reports Rewritten=true without coercing
// the policy-rewrite verdict. The replay machinery is shared between
// policy rewrites and cvreason normalization; this test pins that
// shared substitution path on the cvreason-only branch.
func TestOpenAIChatCompletionsSSEStripsCvReasonWithoutPolicyVerdict(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
	if rewriter == nil {
		t.Fatal("no OpenAI rewriter")
	}

	body := []byte(strings.Join([]string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"src/auth.go\",\"cvreason\":\"locate the login handler\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))

	var seen ToolUse
	res, err := rewriter.Rewrite(body, "text/event-stream", func(tu ToolUse) ToolUseVerdict {
		seen = tu
		return ToolUseVerdict{Allowed: true}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if seen.CvReason != "locate the login handler" {
		t.Errorf("eval CvReason = %q, want extracted text", seen.CvReason)
	}
	if !res.Rewritten {
		t.Errorf("Rewritten=false; want true so downstream drops the stale Content-Length")
	}
	if strings.Contains(string(res.Body), "cvreason") {
		t.Errorf("SSE output retained cvreason: %s", res.Body)
	}
	if !strings.Contains(string(res.Body), "src/auth.go") {
		t.Errorf("SSE output lost real params: %s", res.Body)
	}
}

// TestOpenAIChatCompletionsRewriteJSONFlagsCvReasonRemarshalAsRewritten
// is a regression check for the Rewritten=false bug that shipped in PR
// #541: cvreason-only rewrites must report Rewritten=true so the
// handler at api/handlers/llm_endpoint.go (which gates Content-Length
// and Content-Encoding cleanup on Rewritten) drops the upstream
// headers — otherwise the harness sees the shorter body with the
// upstream's now-stale Content-Length and rejects the response.
func TestOpenAIChatCompletionsRewriteJSONFlagsCvReasonRemarshalAsRewritten(t *testing.T) {
	t.Parallel()
	body := `{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"src/auth.go\",\"cvreason\":\"locate the login handler\"}"}}]},"finish_reason":"tool_calls"}]}`

	eval := func(_ ToolUse) ToolUseVerdict { return ToolUseVerdict{Allowed: true} }
	res, err := (OpenAIResponseRewriter{}).Rewrite([]byte(body), "application/json", eval)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if strings.Contains(string(res.Body), "cvreason") {
		t.Errorf("response body retained cvreason: %s", res.Body)
	}
	if !res.Rewritten {
		t.Errorf("Rewritten=false on cvreason-only re-marshal; body diverges from upstream so handler must drop Content-Length")
	}
}

// TestAnthropicRewriteJSONStripsCvReasonFromBody verifies the
// non-streaming JSON rewriter re-marshals the response body without
// cvreason even when no other rewrite/block occurred.
func TestAnthropicRewriteJSONStripsCvReasonFromBody(t *testing.T) {
	t.Parallel()

	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"src/auth.go","cvreason":"locating login handler"}}],"stop_reason":"tool_use"}`

	var captured ToolUse
	eval := func(tu ToolUse) ToolUseVerdict {
		captured = tu
		return ToolUseVerdict{Allowed: true}
	}

	res, err := (AnthropicResponseRewriter{}).Rewrite([]byte(body), "application/json", eval)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if captured.CvReason != "locating login handler" {
		t.Errorf("eval CvReason = %q, want %q", captured.CvReason, "locating login handler")
	}
	if strings.Contains(string(res.Body), "cvreason") {
		t.Errorf("response body retained cvreason: %s", res.Body)
	}
	if !strings.Contains(string(res.Body), "src/auth.go") {
		t.Errorf("response body lost real params: %s", res.Body)
	}
	// Rewritten must be true: the body bytes diverge from upstream's
	// (cvreason was stripped), so the harness handler needs to know to
	// drop the now-stale Content-Length / Content-Encoding headers.
	// See llm_endpoint.go where `processed.Rewritten` gates that cleanup.
	if !res.Rewritten {
		t.Errorf("Rewritten=false; want true so downstream drops the stale Content-Length")
	}
}
