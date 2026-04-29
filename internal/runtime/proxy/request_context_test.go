package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestBuildRuntimeRequestContextAnthropic(t *testing.T) {
	body := []byte(`{
	  "messages":[
	    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"fetch_messages","input":{"max_results":10}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]},
	    {"role":"user","content":[{"type":"text","text":"continue"}]}
	  ]
	}`)
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	parser := conversation.DefaultRegistry().Match(req)
	if parser == nil {
		t.Fatal("expected anthropic parser")
	}

	ctx := buildRuntimeRequestContext(req, parser, body)
	if ctx == nil {
		t.Fatal("expected runtime request context")
	}
	if ctx.Provider != string(conversation.ProviderAnthropic) {
		t.Fatalf("Provider=%q", ctx.Provider)
	}
	if ctx.RequestPath != "/v1/messages" {
		t.Fatalf("RequestPath=%q", ctx.RequestPath)
	}
	if ctx.RequestBodySHA == "" {
		t.Fatal("expected request body sha")
	}
	if ctx.ParseErr != nil {
		t.Fatalf("ParseErr=%v", ctx.ParseErr)
	}
	if len(ctx.ParsedTurns) != 3 {
		t.Fatalf("ParsedTurns len=%d, want 3", len(ctx.ParsedTurns))
	}
	if len(ctx.ToolResultsSeen) != 1 || ctx.ToolResultsSeen[0] != "toolu_1" {
		t.Fatalf("ToolResultsSeen=%v", ctx.ToolResultsSeen)
	}
}

func TestBuildRuntimeRequestContextOpenAIParseFailureStillHashes(t *testing.T) {
	body := []byte(`{"input":[`)
	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	parser := conversation.DefaultRegistry().Match(req)
	if parser == nil {
		t.Fatal("expected openai parser")
	}

	ctx := buildRuntimeRequestContext(req, parser, body)
	if ctx == nil {
		t.Fatal("expected runtime request context")
	}
	if ctx.RequestBodySHA == "" {
		t.Fatal("expected request body sha")
	}
	if ctx.ParseErr == nil || !strings.Contains(ctx.ParseErr.Error(), "unexpected end") {
		t.Fatalf("ParseErr=%v, want json parse failure", ctx.ParseErr)
	}
}
