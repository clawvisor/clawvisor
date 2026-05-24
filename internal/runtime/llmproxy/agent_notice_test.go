package llmproxy

import (
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestRenderAgentRoutingNotice_NamedAgent(t *testing.T) {
	got := RenderAgentRoutingNotice("My Laptop")
	want := `[Clawvisor] Routing this conversation through Clawvisor as agent "My Laptop".`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAgentRoutingNotice_EmptyName(t *testing.T) {
	got := RenderAgentRoutingNotice("")
	want := `[Clawvisor] Routing this conversation through Clawvisor.`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAgentRoutingNotice_WhitespaceName(t *testing.T) {
	got := RenderAgentRoutingNotice("   \t  ")
	want := `[Clawvisor] Routing this conversation through Clawvisor.`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAgentRoutingNotice_StripsControlChars(t *testing.T) {
	got := RenderAgentRoutingNotice("Line1\nLine2\rEnd")
	want := `[Clawvisor] Routing this conversation through Clawvisor as agent "Line1 Line2 End".`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAgentRoutingNotice_TruncatesLongName(t *testing.T) {
	long := strings.Repeat("z", agentNoticeMaxNameRunes+50)
	got := RenderAgentRoutingNotice(long)
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

func TestRenderAgentRoutingNotice_TruncatesMultibyteRunes(t *testing.T) {
	// Multibyte runes: '日' is 3 bytes. A naive byte-slice would split
	// mid-rune and produce U+FFFD on JSON marshal. Rune-aware truncation
	// keeps the output well-formed.
	long := strings.Repeat("日", agentNoticeMaxNameRunes+10)
	got := RenderAgentRoutingNotice(long)
	if !strings.Contains(got, "…") {
		t.Errorf("expected truncation ellipsis, got %q", got)
	}
	if strings.Count(got, "日") != agentNoticeMaxNameRunes {
		t.Errorf("expected %d 日 runes, got %d", agentNoticeMaxNameRunes, strings.Count(got, "日"))
	}
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

func TestHasInboundAssistantTurn_OpenAINoMessagesOrInput(t *testing.T) {
	body := mustMarshal(t, map[string]any{"model": "gpt-4"})
	if !HasInboundAssistantTurn(conversation.ProviderOpenAI, body) {
		t.Error("expected true (fail-safe) when neither messages nor input present")
	}
}
