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
