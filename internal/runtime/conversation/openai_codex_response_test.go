package conversation

import (
	"os"
	"strings"
	"testing"
)

// Regression: a captured live SSE response from chatgpt.com/backend-api/codex
// must hit the rewriter eval for every function_call output item it carries.
// If the eval never fires, clawvisor.local URLs in the tool_use args won't
// get rewritten and the harness ends up curling a non-existent host.
func TestCodexSSEResponse_RewriterCallsEvalForFunctionCalls(t *testing.T) {
	body, err := os.ReadFile("/tmp/codex_curl_response.txt")
	if err != nil {
		t.Skipf("captured codex response not present at /tmp/codex_response.txt: %v", err)
	}
	if !strings.Contains(string(body), "function_call") {
		t.Fatalf("captured body has no function_call — wrong fixture")
	}
	if !strings.Contains(string(body), "clawvisor.local") {
		t.Fatalf("captured body has no clawvisor.local — wrong fixture")
	}
	var seen []ToolUse
	eval := func(tu ToolUse) ToolUseVerdict {
		seen = append(seen, tu)
		return ToolUseVerdict{Allowed: true}
	}
	rw := OpenAIResponseRewriter{}
	result, err := rw.Rewrite(body, "text/event-stream", eval)
	if err != nil {
		t.Fatalf("Rewrite returned error: %v", err)
	}
	t.Logf("eval invocations: %d", len(seen))
	for i, tu := range seen {
		t.Logf("  #%d name=%s input=%s", i, tu.Name, truncate(string(tu.Input), 200))
	}
	t.Logf("result.Body bytes: %d (input was %d)", len(result.Body), len(body))
	if len(seen) == 0 {
		t.Fatalf("expected the rewriter to call eval at least once for the function_call output items in the captured response; got 0")
	}
}

// Regression: in production we observed Postprocess running with an empty
// Content-Type header (rawlog `content_type=` empty), and tool_use_entry
// trace events never fired. That suggests the rewriter dispatched to the
// JSON path on an SSE body, which decodes to nothing — no eval calls. This
// test asserts that scenario.
func TestCodexSSEResponse_RewriterEmptyContentType(t *testing.T) {
	body, err := os.ReadFile("/tmp/codex_curl_response.txt")
	if err != nil {
		t.Skipf("captured codex response not present at /tmp/codex_curl_response.txt: %v", err)
	}
	var seen []ToolUse
	eval := func(tu ToolUse) ToolUseVerdict {
		seen = append(seen, tu)
		return ToolUseVerdict{Allowed: true}
	}
	rw := OpenAIResponseRewriter{}
	// Pass empty Content-Type — matches what we saw in production.
	_, err = rw.Rewrite(body, "", eval)
	if err != nil {
		t.Fatalf("Rewrite returned error: %v", err)
	}
	t.Logf("eval invocations with empty CT: %d", len(seen))
	if len(seen) == 0 {
		t.Fatalf("rewriter dispatch on empty content-type missed the SSE body's function_calls")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
