package conversation

import (
	"net/http"
	"strings"
	"testing"
)

func TestAnthropicParserParseRequest(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	parser := DefaultRegistry().Match(req)
	if parser == nil {
		t.Fatal("expected Anthropic parser")
	}

	body := []byte(`{
		"system":"Follow task constraints",
		"messages":[
			{"role":"user","content":"List my inbox"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"fetch_messages","input":{"max_results":10}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"10 messages"}]}
		]
	}`)
	turns, err := parser.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if len(turns) != 4 {
		t.Fatalf("len(turns)=%d, want 4", len(turns))
	}
	if turns[0].Role != RoleSystem || turns[1].Role != RoleUser || turns[2].Role != RoleAssistant || turns[3].Role != RoleTool {
		t.Fatalf("unexpected roles: %+v", turns)
	}
	if !strings.Contains(turns[2].Content, "<tool_use name=fetch_messages") {
		t.Fatalf("assistant content missing tool_use marker: %q", turns[2].Content)
	}
	if turns[3].ToolName != "fetch_messages" {
		t.Fatalf("tool result ToolName=%q", turns[3].ToolName)
	}
}

func TestOpenAIParserParseRequest(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	parser := DefaultRegistry().Match(req)
	if parser == nil {
		t.Fatal("expected OpenAI parser")
	}

	body := []byte(`{
		"input":[
			{"type":"message","role":"system","content":[{"type":"text","text":"Stay scoped"}]},
			{"type":"message","role":"user","content":[{"type":"text","text":"Check the ticket queue"}]}
		]
	}`)
	turns, err := parser.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("len(turns)=%d, want 2", len(turns))
	}
	if turns[0].Role != RoleSystem || turns[1].Role != RoleUser {
		t.Fatalf("unexpected roles: %+v", turns)
	}
	if turns[1].Content != "Check the ticket queue" {
		t.Fatalf("unexpected content: %q", turns[1].Content)
	}
}
