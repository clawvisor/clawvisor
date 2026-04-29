package conversation

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderRequestFixturesParseIntoExpectedTurns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		method          string
		rawURL          string
		fixture         string
		wantRoles       []Role
		wantToolName    string
		wantContentPart string
	}{
		{
			name:            "anthropic request with tool result",
			method:          http.MethodPost,
			rawURL:          "https://api.anthropic.com/v1/messages",
			fixture:         "providers/anthropic_messages/request_with_tool_result.json",
			wantRoles:       []Role{RoleSystem, RoleUser, RoleAssistant, RoleTool},
			wantToolName:    "fetch_messages",
			wantContentPart: "<tool_use name=fetch_messages",
		},
		{
			name:            "openai responses request with function output",
			method:          http.MethodPost,
			rawURL:          "https://api.openai.com/v1/responses",
			fixture:         "providers/openai_responses/request_with_function_output.json",
			wantRoles:       []Role{RoleSystem, RoleUser, RoleAssistant, RoleTool},
			wantToolName:    "Bash",
			wantContentPart: "<tool_use name=Bash",
		},
		{
			name:            "openai chat request with tool result",
			method:          http.MethodPost,
			rawURL:          "https://api.openai.com/v1/chat/completions",
			fixture:         "providers/openai_chat/request_with_tool_result.json",
			wantRoles:       []Role{RoleSystem, RoleAssistant, RoleTool},
			wantToolName:    "Bash",
			wantContentPart: "<tool_use name=Bash",
		},
		{
			name:            "codex responses request function loop",
			method:          http.MethodPost,
			rawURL:          "https://chatgpt.com/backend-api/codex/responses",
			fixture:         "providers/codex_responses/request_function_loop.json",
			wantRoles:       []Role{RoleSystem, RoleUser, RoleAssistant, RoleTool},
			wantToolName:    "Bash",
			wantContentPart: "<tool_use name=Bash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, tt.rawURL, nil)
			parser := DefaultRegistry().Match(req)
			if parser == nil {
				t.Fatal("expected parser")
			}
			body := mustReadConversationFixture(t, tt.fixture)
			turns, err := parser.ParseRequest(body)
			if err != nil {
				t.Fatalf("ParseRequest: %v", err)
			}
			if len(turns) != len(tt.wantRoles) {
				t.Fatalf("len(turns)=%d, want %d (%+v)", len(turns), len(tt.wantRoles), turns)
			}
			for i, wantRole := range tt.wantRoles {
				if turns[i].Role != wantRole {
					t.Fatalf("turn[%d].Role=%v, want %v (turn=%+v)", i, turns[i].Role, wantRole, turns[i])
				}
			}
			foundTool := false
			for _, turn := range turns {
				if turn.ToolName == tt.wantToolName {
					foundTool = true
				}
			}
			if tt.wantToolName != "" && !foundTool {
				t.Fatalf("expected tool name %q in turns %+v", tt.wantToolName, turns)
			}
			if tt.wantContentPart != "" && !containsTurnContent(turns, tt.wantContentPart) {
				t.Fatalf("expected turn content containing %q in %+v", tt.wantContentPart, turns)
			}
		})
	}
}

func TestOpenAIResponsesUnsupportedBuiltinsStayUnsupported(t *testing.T) {
	t.Parallel()

	requestFixtures := []string{
		"providers/openai_responses/request_unsupported_shell_call.json",
		"providers/openai_responses/request_unsupported_apply_patch_call.json",
		"providers/openai_responses/request_unsupported_computer_call.json",
		"providers/openai_responses/request_unsupported_mcp_call.json",
	}
	for _, fixture := range requestFixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
			parser := DefaultRegistry().Match(req)
			if parser == nil {
				t.Fatal("expected parser")
			}
			turns, err := parser.ParseRequest(mustReadConversationFixture(t, fixture))
			if err != nil {
				t.Fatalf("ParseRequest: %v", err)
			}
			if len(turns) == 0 {
				t.Fatalf("expected surrounding conversation turns to remain visible, got %+v", turns)
			}
			for _, turn := range turns {
				if turn.Role == RoleTool || turn.ToolName != "" || strings.Contains(turn.Content, "<tool_use") {
					t.Fatalf("unsupported item should not become tool_use marker: %+v", turns)
				}
			}
		})
	}

	responseFixtures := []string{
		"providers/openai_responses/response_unsupported_shell_call.json",
		"providers/openai_responses/response_unsupported_apply_patch_call.json",
		"providers/openai_responses/response_unsupported_computer_call.json",
		"providers/openai_responses/response_unsupported_mcp_call.json",
	}
	for _, fixture := range responseFixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
			resp := &http.Response{}
			rewriter := DefaultResponseRegistry().Match(req, resp)
			if rewriter == nil {
				t.Fatal("expected rewriter")
			}
			body := mustReadConversationFixture(t, fixture)
			result, err := rewriter.Rewrite(body, "application/json", func(ToolUse) ToolUseVerdict {
				return ToolUseVerdict{Allowed: false, Reason: "should not be evaluated"}
			})
			if err != nil {
				t.Fatalf("Rewrite: %v", err)
			}
			if result.Rewritten {
				t.Fatalf("unsupported built-in response should pass through unchanged: %+v", result)
			}
			if len(result.Decisions) != 0 {
				t.Fatalf("unsupported built-in response should not produce decisions: %+v", result.Decisions)
			}
			if string(result.Body) != string(body) {
				t.Fatalf("unsupported built-in response body changed unexpectedly")
			}
		})
	}
}

func TestReplayFixturesExerciseSupportedProviderLoops(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		traceFixture        string
		wantParserProvider  Provider
		wantDecisionName    string
		wantRewriteBlocked  bool
		wantAssistantMarker string
	}{
		{
			name:                "claude code tool loop",
			traceFixture:        "replay/claude_code_tool_loop.json",
			wantParserProvider:  ProviderAnthropic,
			wantDecisionName:    "fetch_messages",
			wantRewriteBlocked:  true,
			wantAssistantMarker: "approve cv-trace",
		},
		{
			name:                "codex function loop",
			traceFixture:        "replay/codex_function_loop.json",
			wantParserProvider:  ProviderOpenAI,
			wantDecisionName:    "Bash",
			wantRewriteBlocked:  true,
			wantAssistantMarker: "approve cv-trace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trace := mustLoadReplayFixture(t, tt.traceFixture)
			req, _ := http.NewRequest(trace.Request.Method, trace.Request.URL, nil)
			parser := DefaultRegistry().Match(req)
			if parser == nil || parser.Name() != tt.wantParserProvider {
				t.Fatalf("expected parser %v, got %v", tt.wantParserProvider, parser)
			}
			requestBody := mustReadConversationFixture(t, trace.Request.BodyFile)
			turns, err := parser.ParseRequest(requestBody)
			if err != nil {
				t.Fatalf("ParseRequest: %v", err)
			}
			if len(turns) == 0 {
				t.Fatal("expected parsed turns")
			}

			rewriter := DefaultResponseRegistry().Match(req, &http.Response{})
			if rewriter == nil {
				t.Fatal("expected response rewriter")
			}
			responseBody := mustReadConversationFixture(t, trace.Response.BodyFile)
			result, err := rewriter.Rewrite(responseBody, trace.Response.ContentType, func(tu ToolUse) ToolUseVerdict {
				if tu.Name != tt.wantDecisionName {
					t.Fatalf("unexpected tool name %q", tu.Name)
				}
				return ToolUseVerdict{
					Allowed:        false,
					Reason:         "requires approval",
					SubstituteWith: "Reply `approve cv-trace` to release this tool call.",
				}
			})
			if err != nil {
				t.Fatalf("Rewrite: %v", err)
			}
			if result.Rewritten != tt.wantRewriteBlocked {
				t.Fatalf("result.Rewritten=%v, want %v", result.Rewritten, tt.wantRewriteBlocked)
			}
			if len(result.Decisions) != 1 || result.Decisions[0].ToolUse.Name != tt.wantDecisionName {
				t.Fatalf("unexpected decisions %+v", result.Decisions)
			}
			if tt.wantAssistantMarker != "" && !strings.Contains(string(result.Body), tt.wantAssistantMarker) {
				t.Fatalf("expected rewritten body to contain %q, got %s", tt.wantAssistantMarker, string(result.Body))
			}
		})
	}
}

type replayFixture struct {
	Request struct {
		Method      string `json:"method"`
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
		BodyFile    string `json:"body_file"`
	} `json:"request"`
	Response struct {
		ContentType string `json:"content_type"`
		BodyFile    string `json:"body_file"`
	} `json:"response"`
}

func mustLoadReplayFixture(t *testing.T, rel string) replayFixture {
	t.Helper()
	var trace replayFixture
	b := mustReadConversationFixture(t, rel)
	if err := json.Unmarshal(b, &trace); err != nil {
		t.Fatalf("unmarshal replay fixture %s: %v", rel, err)
	}
	return trace
}

func mustReadConversationFixture(t *testing.T, rel string) []byte {
	t.Helper()
	path := filepath.Join("testdata", filepath.FromSlash(rel))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	return b
}

func containsTurnContent(turns []Turn, part string) bool {
	for _, turn := range turns {
		if strings.Contains(turn.Content, part) {
			return true
		}
	}
	return false
}
