package conversation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnthropicResponseRewriterAllowsToolUseJSON(t *testing.T) {
	t.Parallel()

	body := []byte(`{
	  "id":"msg_1",
	  "type":"message",
	  "role":"assistant",
	  "model":"claude-test",
	  "content":[{"type":"tool_use","id":"toolu_1","name":"fetch_messages","input":{"max_results":10}}],
	  "stop_reason":"tool_use"
	}`)

	result, err := (&AnthropicResponseRewriter{}).Rewrite(body, "application/json", func(tu ToolUse) ToolUseVerdict {
		if tu.Name != "fetch_messages" {
			t.Fatalf("unexpected tool name %q", tu.Name)
		}
		return ToolUseVerdict{Allowed: true}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if result.Rewritten {
		t.Fatal("expected passthrough response")
	}
	if len(result.Decisions) != 1 {
		t.Fatalf("expected one decision, got %d", len(result.Decisions))
	}
	if result.AssistantTurn == nil || !strings.Contains(result.AssistantTurn.Content, "<tool_use name=fetch_messages") {
		t.Fatalf("assistant turn missing tool marker: %+v", result.AssistantTurn)
	}
}

func TestAnthropicResponseRewriterBlocksToolUseJSON(t *testing.T) {
	t.Parallel()

	body := []byte(`{
	  "id":"msg_2",
	  "type":"message",
	  "role":"assistant",
	  "model":"claude-test",
	  "content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"rm -rf /"}}],
	  "stop_reason":"tool_use"
	}`)

	result, err := (&AnthropicResponseRewriter{}).Rewrite(body, "application/json", func(ToolUse) ToolUseVerdict {
		return ToolUseVerdict{
			Allowed:        false,
			Reason:         "requires approval",
			SubstituteWith: "Reply `approve cv-test` to release this tool call.",
		}
	})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected rewritten response")
	}
	var out map[string]any
	if err := json.Unmarshal(result.Body, &out); err != nil {
		t.Fatalf("unmarshal rewritten response: %v", err)
	}
	content := out["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "approve cv-test") {
		t.Fatalf("expected inline approval prompt, got %q", text)
	}
}

func TestAnthropicToolResultIDsFromRequest(t *testing.T) {
	t.Parallel()

	body := []byte(`{
	  "messages":[
	    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"fetch_messages","input":{"max_results":10}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
	  ]
	}`)

	ids := AnthropicToolResultIDsFromRequest(body)
	if len(ids) != 1 || ids[0] != "toolu_1" {
		t.Fatalf("unexpected tool result ids: %v", ids)
	}
}
