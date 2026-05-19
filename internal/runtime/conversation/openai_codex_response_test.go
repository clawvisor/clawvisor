package conversation

import (
	"os"
	"strings"
	"testing"
)

// Regression: a Codex SSE response from chatgpt.com/backend-api/codex must
// hit the rewriter's eval callback for every function_call output item so
// clawvisor.local control URLs in tool_use arguments get rewritten before
// the harness executes them.
//
// The fixture is a minimal hand-crafted SSE turn that mirrors the live
// chatgpt.com shape: response.output_item.added (function_call type) →
// function_call_arguments.done → response.output_item.done. Synthetic
// because production responses contain user prompts we shouldn't ship in
// the repo, and because a hand-crafted fixture is easier to keep stable
// when OpenAI updates its event vocabulary.
func TestCodexSSEResponse_RewriterCallsEvalForFunctionCalls(t *testing.T) {
	body := loadCodexFixture(t)
	if !strings.Contains(string(body), "function_call") {
		t.Fatalf("fixture has no function_call — corrupted?")
	}
	if !strings.Contains(string(body), "clawvisor.local") {
		t.Fatalf("fixture has no clawvisor.local — corrupted?")
	}
	var seen []ToolUse
	eval := func(tu ToolUse) ToolUseVerdict {
		seen = append(seen, tu)
		return ToolUseVerdict{Allowed: true}
	}
	rw := OpenAIResponseRewriter{}
	if _, err := rw.Rewrite(body, "text/event-stream", eval); err != nil {
		t.Fatalf("Rewrite returned error: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("expected the rewriter to call eval at least once for the fixture's function_call output items; got 0")
	}
	if !strings.Contains(string(seen[0].Input), "clawvisor.local") {
		t.Errorf("expected first function_call's input to contain clawvisor.local, got %s", truncate(string(seen[0].Input), 200))
	}
}

// Regression: production traffic arrives with the Content-Type header
// missing on some proxy paths. Earlier the rewriter saw an SSE body but
// the dispatch picked the JSON path on empty Content-Type, json.Unmarshal
// silently failed, and zero tool_uses were extracted — clawvisor.local
// URLs went through to the harness un-rewritten. The fallback in Rewrite
// now sniffs SSE framing from the body when Content-Type is empty.
func TestCodexSSEResponse_RewriterEmptyContentType(t *testing.T) {
	body := loadCodexFixture(t)
	var seen []ToolUse
	eval := func(tu ToolUse) ToolUseVerdict {
		seen = append(seen, tu)
		return ToolUseVerdict{Allowed: true}
	}
	rw := OpenAIResponseRewriter{}
	if _, err := rw.Rewrite(body, "", eval); err != nil {
		t.Fatalf("Rewrite returned error: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("rewriter must sniff SSE framing from the body and dispatch to the SSE path when Content-Type is missing")
	}
}

func loadCodexFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/codex/responses_clawvisor_task.sse")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return body
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
