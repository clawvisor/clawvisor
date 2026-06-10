package conversation

import (
	"bytes"
	"context"
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
	// Rewritten should NOT be set just because we stripped cvreason —
	// that flag signals a policy decision rewrote inputs.
	if res.Rewritten {
		t.Errorf("Rewritten=true; want false for cvreason-only re-marshal")
	}
}
