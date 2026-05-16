package llmproxy

import (
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestStripSecretDecisionHistory_AnthropicRemovesPromptAndDecision(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"token ghp_example1234567890abcdef"},{"role":"assistant","content":[{"type":"text","text":"Clawvisor detected a possible raw secret.\n\n[clawvisor:secret=cv-secret-1]"}]},{"role":"user","content":"vault github_ci"},{"role":"user","content":"continue"}]}`)
	got, err := StripSecretDecisionHistory(SecretDecisionHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("StripSecretDecisionHistory: %v", err)
	}
	if !got.Modified {
		t.Fatalf("expected decision history to be stripped")
	}
	text := string(got.Body)
	for _, forbidden := range []string{"Clawvisor detected a possible raw secret", "[clawvisor:secret=", "vault github_ci"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("expected %q stripped from %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "token ghp_example1234567890abcdef") || !strings.Contains(text, "continue") {
		t.Fatalf("expected surrounding user history preserved: %s", text)
	}
}

func TestStripSecretDecisionHistory_NoOpWithoutPrompt(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"vault github_ci"}]}`)
	got, err := StripSecretDecisionHistory(SecretDecisionHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("StripSecretDecisionHistory: %v", err)
	}
	if got.Modified {
		t.Fatalf("standalone vault text should not be stripped")
	}
	if string(got.Body) != string(body) {
		t.Fatalf("no-op should preserve bytes, got %s", got.Body)
	}
}

func TestStripSecretDecisionHistory_OpenAIResponsesOutputText(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"token sk-test1234567890abcdef"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Clawvisor detected a possible raw secret.\n\n[clawvisor:secret=cv-secret-1]"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"vault resend_1"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)
	got, err := StripSecretDecisionHistory(SecretDecisionHistoryStripRequest{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("StripSecretDecisionHistory: %v", err)
	}
	if !got.Modified {
		t.Fatalf("expected Responses decision history to be stripped")
	}
	text := string(got.Body)
	for _, forbidden := range []string{"Clawvisor detected a possible raw secret", "[clawvisor:secret=", "vault resend_1"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("expected %q stripped from %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "token sk-test1234567890abcdef") || !strings.Contains(text, "continue") {
		t.Fatalf("expected surrounding user history preserved: %s", text)
	}
}
