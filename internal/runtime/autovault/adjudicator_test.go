package autovault

import (
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/config"
)

func TestBuildSecretAdjudicatorPromptRedactsPeerCandidates(t *testing.T) {
	current := "AmbiguousCurrent_8gyXD1ddhvF8iEFwrt9f3ywd"
	peer := "AmbiguousPeer_9hyYE2eeivG9jFGxsu0g4zxe"
	content := "The request mentioned " + current + " and another possible credential " + peer + "."

	prompt := BuildSecretAdjudicatorPrompt("api.example.test", "content", content, Candidate{
		Value:   current,
		Charset: "mixed",
		Entropy: 4.2,
	})

	if strings.Contains(prompt, current) {
		t.Fatalf("current candidate should be redacted before adjudication:\n%s", prompt)
	}
	if strings.Contains(prompt, peer) {
		t.Fatalf("peer candidate should also be redacted before adjudication:\n%s", prompt)
	}
}

// TestParseSecretAdjudicatorVerdict_RejectsCanaryMismatch is a
// regression test for the prompt-injection mitigation. A response that
// omits or alters the canary must be refused — converting a successful
// injection into a fail-closed "no decision" rather than a false
// negative on credential detection.
func TestParseSecretAdjudicatorVerdict_RejectsCanaryMismatch(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{
			name: "canary present and correct",
			raw:  `{"credential":true,"service":"github","confidence":0.9,"canary":"` + adjudicatorPromptCanary + `"}`,
			ok:   true,
		},
		{
			name: "canary missing",
			raw:  `{"credential":false,"service":"","confidence":0.1}`,
			ok:   false,
		},
		{
			name: "canary wrong",
			raw:  `{"credential":false,"service":"","confidence":0.1,"canary":"attacker-supplied"}`,
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseSecretAdjudicatorVerdict(c.raw)
			if c.ok && err != nil {
				t.Fatalf("expected success, got err=%v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("expected canary-mismatch error, got nil")
			}
		})
	}
}

// TestBuildSecretAdjudicatorPrompt_FencesUntrustedFields ensures that
// the attacker-influenced fields (host, fieldName, content) appear
// inside the BEGIN/END sentinel fence so the model is instructed to
// treat them as data.
func TestBuildSecretAdjudicatorPrompt_FencesUntrustedFields(t *testing.T) {
	prompt := BuildSecretAdjudicatorPrompt(
		"api.example.test",
		"x-evil-header",
		"surrounding ctx",
		Candidate{Value: "v", Charset: "alphanum", Entropy: 3.0},
	)
	for _, fragment := range []string{
		"[BEGIN HOST]",
		"[END HOST]",
		"[BEGIN FIELD]",
		"[END FIELD]",
		"[BEGIN REDACTED CONTEXT]",
		"[END REDACTED CONTEXT]",
		adjudicatorPromptCanary,
	} {
		if !strings.Contains(prompt, fragment) {
			t.Errorf("prompt is missing required fragment %q\nprompt:\n%s", fragment, prompt)
		}
	}
}

func TestSecretAdjudicatorConfigured(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.VerificationConfig
		want bool
	}{
		{
			name: "disabled",
			cfg: config.VerificationConfig{
				LLMProviderConfig: config.LLMProviderConfig{
					Provider: "openai",
					Endpoint: "https://api.openai.com/v1",
					Model:    "gpt-test",
				},
			},
			want: false,
		},
		{
			name: "endpoint provider",
			cfg: config.VerificationConfig{
				LLMProviderConfig: config.LLMProviderConfig{
					Enabled:  true,
					Provider: "openai",
					Endpoint: "https://api.openai.com/v1",
					Model:    "gpt-test",
				},
			},
			want: true,
		},
		{
			name: "gemini project region endpoint built by client",
			cfg: config.VerificationConfig{
				LLMProviderConfig: config.LLMProviderConfig{
					Enabled:  true,
					Provider: "gemini",
					Project:  "clawvisor-staging",
					Region:   "global",
					Model:    "gemini-3.1-flash-lite-preview",
				},
			},
			want: true,
		},
		{
			name: "gemini requires project when endpoint omitted",
			cfg: config.VerificationConfig{
				LLMProviderConfig: config.LLMProviderConfig{
					Enabled:  true,
					Provider: "gemini",
					Model:    "gemini-3.1-flash-lite-preview",
				},
			},
			want: false,
		},
		{
			name: "non gemini requires endpoint",
			cfg: config.VerificationConfig{
				LLMProviderConfig: config.LLMProviderConfig{
					Enabled:  true,
					Provider: "openai",
					Model:    "gpt-test",
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SecretAdjudicatorConfigured(tt.cfg); got != tt.want {
				t.Fatalf("SecretAdjudicatorConfigured() = %v, want %v", got, tt.want)
			}
		})
	}
}
