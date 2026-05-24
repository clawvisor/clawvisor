package llmproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestExtractRecentHumanTurns_AnthropicSimpleStringContent(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "summarize my inbox"},
			{"role": "assistant", "content": "ok"},
		},
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if len(turns) != 1 || turns[0] != "summarize my inbox" {
		t.Errorf("turns = %v, want [summarize my inbox]", turns)
	}
}

func TestExtractRecentHumanTurns_AnthropicBlockContent(t *testing.T) {
	// Mixed-block content: text + tool_result. Tool result must be
	// filtered out; the text block is the genuine human input.
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "please draft the email"},
					{"type": "tool_result", "tool_use_id": "x", "content": "{}"},
				},
			},
		},
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if len(turns) != 1 || turns[0] != "please draft the email" {
		t.Errorf("turns = %v, want [please draft the email]", turns)
	}
}

func TestExtractRecentHumanTurns_AnthropicSkipsToolResultOnly(t *testing.T) {
	// A user-role message that contains only tool_result blocks is
	// harness output, not human input — must be skipped entirely.
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "real ask"},
			{"role": "assistant", "content": "ok"},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "x", "content": "{}"},
				},
			},
		},
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if len(turns) != 1 || turns[0] != "real ask" {
		t.Errorf("turns = %v, want [real ask]", turns)
	}
}

func TestExtractRecentHumanTurns_AnthropicSkipsBareApprovalVerbs(t *testing.T) {
	// "yes" / "no" / "task" replies are users driving the approval
	// flow itself — they must NOT be treated as authorization for the
	// underlying work.
	for _, verb := range []string{"yes", "y", "no", "n", "task", "  YES  "} {
		body := mustMarshal(t, map[string]any{
			"messages": []map[string]any{
				{"role": "user", "content": "summarize my inbox"},
				{"role": "assistant", "content": "ok"},
				{"role": "user", "content": verb},
			},
		})
		turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
			Provider: conversation.ProviderAnthropic,
			Body:     body,
		})
		if len(turns) != 1 || turns[0] != "summarize my inbox" {
			t.Errorf("verb=%q: turns = %v, want [summarize my inbox]", verb, turns)
		}
	}
}

func TestExtractRecentHumanTurns_AnthropicSkipsAugmentedReply(t *testing.T) {
	// When the proxy augments a user's "yes" with an internal marker
	// payload, the resulting user-role text is Clawvisor-internal and
	// must be filtered. We test with the literal augmented marker
	// string so we don't bake assumptions about the exact wording.
	augmented := "yes" + "\n\n" + InlineTaskDenyMarker + "cv-task-abc]"
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "the real ask"},
			{"role": "assistant", "content": "ok"},
			{"role": "user", "content": augmented},
		},
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if len(turns) != 1 || turns[0] != "the real ask" {
		t.Errorf("turns = %v, want [the real ask]", turns)
	}
}

func TestExtractRecentHumanTurns_AnthropicMultipleTurnsOrdered(t *testing.T) {
	// Most recent last, capped at maxRecentHumanTurns. Four turns
	// should collapse to the last three.
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "first"},
			{"role": "assistant", "content": "a"},
			{"role": "user", "content": "second"},
			{"role": "assistant", "content": "b"},
			{"role": "user", "content": "third"},
			{"role": "assistant", "content": "c"},
			{"role": "user", "content": "fourth"},
		},
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	want := []string{"second", "third", "fourth"}
	if len(turns) != len(want) {
		t.Fatalf("turns = %v, want %v", turns, want)
	}
	for i := range want {
		if turns[i] != want[i] {
			t.Errorf("turns[%d] = %q, want %q", i, turns[i], want[i])
		}
	}
}

func TestExtractRecentHumanTurns_EmptyBody(t *testing.T) {
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     nil,
	})
	if len(turns) != 0 {
		t.Errorf("turns = %v, want []", turns)
	}
}

func TestExtractRecentHumanTurns_MalformedJSON(t *testing.T) {
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     []byte("{not json"),
	})
	if len(turns) != 0 {
		t.Errorf("turns = %v, want []", turns)
	}
}

func TestExtractRecentHumanTurns_NoUserMessages(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "assistant", "content": "hello"},
		},
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if len(turns) != 0 {
		t.Errorf("turns = %v, want []", turns)
	}
}

func TestExtractRecentHumanTurns_UnknownProvider(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "ask"},
		},
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.Provider("unknown"),
		Body:     body,
	})
	if len(turns) != 0 {
		t.Errorf("turns = %v, want []", turns)
	}
}

func TestExtractRecentHumanTurns_AnthropicWhitespaceOnly(t *testing.T) {
	// A whitespace-only user message is not a genuine turn — must
	// not appear in the output.
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "real ask"},
			{"role": "user", "content": "   \n\t  "},
		},
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if len(turns) != 1 || turns[0] != "real ask" {
		t.Errorf("turns = %v, want [real ask]", turns)
	}
}

func TestExtractRecentHumanTurns_AnthropicLongTurnPreserved(t *testing.T) {
	// Don't truncate individual turn content — the assessor needs the
	// full text to judge intent. Capping happens at the count level.
	long := strings.Repeat("a", 5000)
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": long},
		},
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if len(turns) != 1 || turns[0] != long {
		t.Errorf("long turn not preserved verbatim")
	}
}

func TestExtractRecentHumanTurns_OpenAIChatCompletions(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"messages": []map[string]any{
			{"role": "system", "content": "you are a bot"},
			{"role": "user", "content": "delete spam"},
			{"role": "assistant", "content": "ok"},
			{"role": "user", "content": "yes"},
		},
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	// "yes" filtered; "delete spam" remains.
	if len(turns) != 1 || turns[0] != "delete spam" {
		t.Errorf("turns = %v, want [delete spam]", turns)
	}
}

func TestExtractRecentHumanTurns_OpenAIResponsesAPI(t *testing.T) {
	body := mustMarshal(t, map[string]any{
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "schedule a meeting"},
			{"type": "function_call", "name": "schedule", "arguments": "{}"},
		},
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if len(turns) != 1 || turns[0] != "schedule a meeting" {
		t.Errorf("turns = %v, want [schedule a meeting]", turns)
	}
}

func TestExtractRecentHumanTurns_OpenAIResponsesAPIStringInput(t *testing.T) {
	// {"input": "run echo"} is the convenience form the Responses API
	// accepts when there's no prior history. The extractor must treat
	// the bare string as a single human turn — otherwise
	// conversation-based auto-approval is unreachable for any client
	// using this shape (which is the default for one-shot completions).
	body := mustMarshal(t, map[string]any{
		"input": "run echo",
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if len(turns) != 1 || turns[0] != "run echo" {
		t.Errorf("turns = %v, want [run echo]", turns)
	}
}

func TestExtractRecentHumanTurns_OpenAIResponsesAPIStringInputBareVerb(t *testing.T) {
	// Bare approval verbs must still be filtered out even in the
	// string-input form — they're internal Clawvisor traffic, not
	// fresh human authorization.
	body := mustMarshal(t, map[string]any{
		"input": "yes",
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if len(turns) != 0 {
		t.Errorf("turns = %v, want [] (bare approval verb)", turns)
	}
}

func TestExtractRecentHumanTurns_OpenAIResponsesAPIEmptyStringInput(t *testing.T) {
	// Whitespace-only string input must not produce an empty turn.
	body := mustMarshal(t, map[string]any{
		"input": "   \n  ",
	})
	turns := ExtractRecentHumanTurns(ExtractHumanTurnsRequest{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if len(turns) != 0 {
		t.Errorf("turns = %v, want []", turns)
	}
}

func TestTailLimit(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		n    int
		want []string
	}{
		{"nil", nil, 3, []string{}},
		{"empty", []string{}, 3, []string{}},
		{"under_limit", []string{"a", "b"}, 3, []string{"a", "b"}},
		{"at_limit", []string{"a", "b", "c"}, 3, []string{"a", "b", "c"}},
		{"over_limit", []string{"a", "b", "c", "d", "e"}, 3, []string{"c", "d", "e"}},
		{"zero_n", []string{"a", "b"}, 0, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tailLimit(tc.in, tc.n)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got %v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
