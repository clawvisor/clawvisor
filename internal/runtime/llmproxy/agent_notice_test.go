package llmproxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestRenderAgentRoutingNotice_NamedAgent(t *testing.T) {
	got := RenderAgentRoutingNotice("My Laptop", "")
	want := "`[Clawvisor] Routing this conversation through Clawvisor as agent \"My Laptop\".`"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAgentRoutingNotice_EmptyName(t *testing.T) {
	got := RenderAgentRoutingNotice("", "")
	want := "`[Clawvisor] Routing this conversation through Clawvisor.`"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAgentRoutingNotice_WhitespaceName(t *testing.T) {
	got := RenderAgentRoutingNotice("   \t  ", "")
	want := "`[Clawvisor] Routing this conversation through Clawvisor.`"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAgentRoutingNotice_StripsControlChars(t *testing.T) {
	got := RenderAgentRoutingNotice("Line1\nLine2\rEnd", "")
	want := "`[Clawvisor] Routing this conversation through Clawvisor as agent \"Line1 Line2 End\".`"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAgentRoutingNotice_TruncatesLongName(t *testing.T) {
	long := strings.Repeat("z", agentNoticeMaxNameRunes+50)
	got := RenderAgentRoutingNotice(long, "")
	if !strings.Contains(got, "…") {
		t.Errorf("expected truncation ellipsis, got %q", got)
	}
	// 'z' appears nowhere in the surrounding template, so counting
	// occurrences in the rendered notice gives the truncated name's
	// rune length directly.
	if strings.Count(got, "z") != agentNoticeMaxNameRunes {
		t.Errorf("expected %d z's, got %d (notice=%q)", agentNoticeMaxNameRunes, strings.Count(got, "z"), got)
	}
}

// TestRenderAgentRoutingNotice_StripsBackticks pins the defense that
// keeps an agent name from terminating the markdown inline-code span
// the notice is wrapped in. The agent name is operator-controlled so
// this is defense-in-depth, but a stray backtick would still leak
// the trailing guidance as prose.
func TestRenderAgentRoutingNotice_StripsBackticks(t *testing.T) {
	got := RenderAgentRoutingNotice("agent`name`with`ticks", "")
	if strings.Count(got, "`") != 2 {
		t.Errorf("expected exactly the two wrapping backticks; got %q", got)
	}
	if strings.Contains(got, "agent`") {
		t.Errorf("expected backticks stripped from agent name; got %q", got)
	}
}

func TestRenderAgentRoutingNotice_TruncatesMultibyteRunes(t *testing.T) {
	// Multibyte runes: '日' is 3 bytes. A naive byte-slice would split
	// mid-rune and produce U+FFFD on JSON marshal. Rune-aware truncation
	// keeps the output well-formed.
	long := strings.Repeat("日", agentNoticeMaxNameRunes+10)
	got := RenderAgentRoutingNotice(long, "")
	if !strings.Contains(got, "…") {
		t.Errorf("expected truncation ellipsis, got %q", got)
	}
	if strings.Count(got, "日") != agentNoticeMaxNameRunes {
		t.Errorf("expected %d 日 runes, got %d", agentNoticeMaxNameRunes, strings.Count(got, "日"))
	}
}

func TestRenderAgentRoutingNotice_AppendsConversationIDMarker(t *testing.T) {
	id := "cv-conv-abcdefghijklmnopqrstuvwxyz"
	got := RenderAgentRoutingNotice("My Laptop", id)
	want := "`[Clawvisor] Routing this conversation through Clawvisor as agent \"My Laptop\".` [clawvisor:conversation=cv-conv-abcdefghijklmnopqrstuvwxyz]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAgentRoutingNotice_AppendsMarkerToNameLessFallback(t *testing.T) {
	got := RenderAgentRoutingNotice("", "cv-conv-aaaaaaaaaaaaaaaaaaaaaaaaaa")
	want := "`[Clawvisor] Routing this conversation through Clawvisor.` [clawvisor:conversation=cv-conv-aaaaaaaaaaaaaaaaaaaaaaaaaa]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAgentRoutingNotice_WhitespaceMintedIDOmitsMarker(t *testing.T) {
	// Empty/whitespace mintedConversationID must not produce an empty
	// "[clawvisor:conversation=]" footer.
	got := RenderAgentRoutingNotice("agent", "   ")
	want := "`[Clawvisor] Routing this conversation through Clawvisor as agent \"agent\".`"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAgentRoutingNotice_MintedIDRoundTripsThroughScanner(t *testing.T) {
	id, err := conversation.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	notice := RenderAgentRoutingNotice("agent", id)
	// Build a Chat Completions inbound body where the assistant turn
	// is exactly the rendered notice — simulating turn-2 echo.
	body := []byte(`{"messages":[{"role":"assistant","content":` + mustJSONString(t, notice) + `}]}`)
	req := chatCompletionsReq(t)
	recovered := conversation.FindInjectedConversationID(req, conversation.ProviderOpenAI, body)
	if recovered != id {
		t.Fatalf("rendered notice did not round-trip through scanner: minted=%q recovered=%q notice=%q", id, recovered, notice)
	}
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func chatCompletionsReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestHasInboundAssistantTurn_AnthropicNoAssistant(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	if HasInboundAssistantTurn(conversation.ProviderAnthropic, body) {
		t.Error("expected false for user-only inbound")
	}
}

func TestHasInboundAssistantTurn_AnthropicWithAssistant(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"},
			{"role": "user", "content": "follow-up"},
		},
	})
	if !HasInboundAssistantTurn(conversation.ProviderAnthropic, body) {
		t.Error("expected true when inbound has prior assistant turn")
	}
}

func TestHasInboundAssistantTurn_AnthropicEmptyMessages(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{},
	})
	if HasInboundAssistantTurn(conversation.ProviderAnthropic, body) {
		t.Error("expected false for empty messages[]")
	}
}

func TestHasInboundAssistantTurn_AnthropicNoMessagesField(t *testing.T) {
	body := mustMarshal(t, map[string]any{"model": "claude"})
	if !HasInboundAssistantTurn(conversation.ProviderAnthropic, body) {
		t.Error("expected true (fail-safe) when messages[] absent")
	}
}

func TestHasInboundAssistantTurn_MalformedJSON(t *testing.T) {
	if !HasInboundAssistantTurn(conversation.ProviderAnthropic, []byte("{not json")) {
		t.Error("expected true (fail-safe) on malformed body")
	}
}

func TestHasInboundAssistantTurn_EmptyBody(t *testing.T) {
	if !HasInboundAssistantTurn(conversation.ProviderAnthropic, nil) {
		t.Error("expected true (fail-safe) on empty body")
	}
}

func TestHasInboundAssistantTurn_UnknownProvider(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if !HasInboundAssistantTurn(conversation.Provider("unknown"), body) {
		t.Error("expected true (fail-safe) for unknown provider")
	}
}

func TestHasInboundAssistantTurn_OpenAIChatNoAssistant(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "system", "content": "you are a helper"},
			{"role": "user", "content": "hi"},
		},
	})
	if HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected false for system+user OpenAI Chat inbound")
	}
}

func TestHasInboundAssistantTurn_OpenAIChatWithAssistant(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"},
		},
	})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true when OpenAI Chat inbound has assistant turn")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesStringInput(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"input": "run echo hello",
	})
	if HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected false for OpenAI Responses string input (no prior history)")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesArrayNoAssistant(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "hi"},
		},
	})
	if HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected false for OpenAI Responses user-only array input")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesArrayWithAssistant(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "hi"},
			{"type": "message", "role": "assistant", "content": "hello"},
		},
	})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true when OpenAI Responses array has assistant turn")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesFunctionCallHistory(t *testing.T) {
	// Turn-2+ Responses continuation: the assistant's prior turn was a
	// tool call (function_call) followed by the tool's output
	// (function_call_output). No assistant-role message exists. Must
	// still be treated as "has assistant history" so the routing notice
	// doesn't re-prepend on every continuation.
	body := mustMarshal(t, map[string]any{
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "list files"},
			{"type": "function_call", "name": "Bash", "arguments": `{"command":"ls"}`, "call_id": "call_1"},
			{"type": "function_call_output", "call_id": "call_1", "output": "a\nb"},
		},
	})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true when Responses input carries function_call history")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesReasoningHistory(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "think then answer"},
			{"type": "reasoning", "summary": []any{}, "encrypted_content": "..."},
		},
	})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true when Responses input carries reasoning history")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesBuiltinToolCallHistory(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "search the web"},
			{"type": "web_search_call", "id": "ws_1"},
		},
	})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true when Responses input carries a built-in tool_call item")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesCustomToolCallHistory(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "use my tool"},
			{"type": "custom_tool_call", "name": "do_thing", "call_id": "ct_1"},
		},
	})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true when Responses input carries a custom_tool_call item")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesSystemAndUserOnly(t *testing.T) {
	// First turn variants: developer/system priming + user message,
	// no prior assistant activity of any kind. Must still detect as
	// first turn.
	body := mustMarshal(t, map[string]any{
		"input": []map[string]any{
			{"type": "message", "role": "system", "content": "you are a helper"},
			{"type": "message", "role": "developer", "content": "behave deterministically"},
			{"type": "message", "role": "user", "content": "hi"},
		},
	})
	if HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected false for system/developer/user-only Responses input")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesItemWithoutTypeIsMessage(t *testing.T) {
	// Items default to type:"message" when omitted. A user-role item
	// without an explicit type must still be classified correctly.
	body := mustMarshal(t, map[string]any{
		"input": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	if HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected false when message item omits type field (defaults to message)")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesPreviousResponseIDStringInput(t *testing.T) {
	// Stateful Responses follow-up: the harness chains turns via
	// previous_response_id and ships only the new user turn as a plain
	// string input. Server-side state holds the assistant history, so
	// this must NOT be treated as a first turn.
	body := mustMarshal(t, map[string]any{
		"previous_response_id": "resp_abc123",
		"input":                "follow up question",
	})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true when previous_response_id is set (stateful follow-up)")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesPreviousResponseIDUserItems(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"previous_response_id": "resp_abc123",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "another follow up"},
		},
	})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true when previous_response_id is set with user-only input items")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesEmptyPreviousResponseID(t *testing.T) {
	// Empty/null previous_response_id must NOT trigger the follow-up
	// classification — it's the absent-field signal.
	for _, tc := range []struct {
		name string
		val  any
	}{
		{"empty string", ""},
		{"null", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := mustMarshal(t, map[string]any{
				"previous_response_id": tc.val,
				"input":                "first turn",
			})
			if HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
				t.Errorf("expected false when previous_response_id is %v", tc.val)
			}
		})
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesConversationAloneNotConclusive(t *testing.T) {
	// `conversation` is a container, not a back-reference: a fresh
	// client kickoff can ship a brand-new (empty) conversation alongside
	// the user's first turn. Without other evidence of prior assistant
	// activity, this must be classified as a first turn so the routing
	// notice fires. Compare to previous_response_id, which IS a
	// conclusive back-reference and short-circuits to "has history."
	for _, tc := range []struct {
		name string
		conv any
	}{
		{"string form", "conv_xyz789"},
		{"object form", map[string]any{"id": "conv_xyz789"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := mustMarshal(t, map[string]any{
				"conversation": tc.conv,
				"input":        "first turn in this conversation",
			})
			if HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
				t.Errorf("expected false (first turn) for conversation-only kickoff, conv=%v", tc.conv)
			}
		})
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesConversationWithPreviousResponseID(t *testing.T) {
	// Mixed-field follow-up: conversation container + previous_response_id
	// back-reference. The back-reference is conclusive — there IS a
	// prior response.
	body := mustMarshal(t, map[string]any{
		"conversation":         "conv_xyz789",
		"previous_response_id": "resp_abc123",
		"input":                "follow up",
	})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true when previous_response_id is set alongside conversation")
	}
}

func TestHasInboundAssistantTurn_OpenAIResponsesConversationWithAssistantInput(t *testing.T) {
	// A conversation chain that echoes prior assistant items in
	// `input` is still caught by the items walk — `conversation`
	// being non-conclusive doesn't disable the rest of the detector.
	body := mustMarshal(t, map[string]any{
		"conversation": "conv_xyz789",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "first"},
			{"type": "function_call", "name": "Bash", "arguments": `{"command":"ls"}`, "call_id": "c1"},
			{"type": "function_call_output", "call_id": "c1", "output": "a"},
			{"type": "message", "role": "user", "content": "follow up"},
		},
	})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true when conversation chain echoes prior assistant items in input")
	}
}

func TestHasInboundAssistantTurn_OpenAINoMessagesOrInput(t *testing.T) {
	body := mustMarshal(t, map[string]any{"model": "gpt-4"})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true (fail-safe) when neither messages nor input present")
	}
}

// TestHasInboundAssistantTurn_PostSyntheticApprovalStripIsAmbiguous documents
// the bug class the handler avoids by snapshotting the body BEFORE
// StripSyntheticApprovalHistory runs.
//
// Scenario: a fresh conversation's very first user prompt triggered a
// tool_use that Postprocess substituted with an approval prompt
// containing ToolApprovalSubstitutedPromptMarker. The user replied
// "approve". On the continuation request, the inbound body has
// [user, assistant(prompt+routing-notice), user("approve")]. The
// strip removes the assistant prompt AND the bare reply, leaving just
// [user]. The detector cannot tell this stripped body apart from a
// genuine fresh turn-1 request and (correctly per its own contract)
// returns false.
//
// The handler must therefore pass the pre-strip body to the detector
// (or otherwise consult the synthetic_approval_history_stripped audit
// signal). This test pins that invariant: if you change the strip to
// preserve a turn-marker stub, update both this test and the handler
// snapshot logic together.
func TestHasInboundAssistantTurn_PostSyntheticApprovalStripIsAmbiguous(t *testing.T) {
	preStrip := anthropicTextBody(
		map[string]string{"role": "user", "content": "delete /tmp/x"},
		map[string]string{"role": "assistant", "content": "[Clawvisor] Routing this conversation through Clawvisor as agent \"laptop\".\n\n" + ToolApprovalSubstitutedPromptMarker + " Reply yes or y to approve."},
		map[string]string{"role": "user", "content": "y"},
	)
	// Sanity: the un-stripped body has the assistant turn the user saw.
	if !HasInboundAssistantTurn(conversation.ProviderAnthropic, preStrip) {
		t.Fatal("pre-strip body should detect as having an assistant turn")
	}
	stripped, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     preStrip,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !stripped.Modified {
		t.Fatal("expected the synthetic approval prompt to be stripped")
	}
	// Post-strip the assistant turn is gone — detection now reports
	// "no assistant" even though the user observed one in turn 1.
	// The handler MUST use the pre-strip body for the first-turn
	// decision; this assertion locks the failure mode in place so a
	// future refactor that loses the snapshot trips this test.
	if HasInboundAssistantTurn(conversation.ProviderAnthropic, stripped.Body) {
		t.Fatal("post-strip body unexpectedly still has an assistant turn; the snapshot may no longer be necessary — re-evaluate the handler wiring")
	}
}
