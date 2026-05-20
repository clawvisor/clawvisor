package handlers

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/config"
)

func TestWelcomeSuggestionsProviderConfigFallsBackToConfiguredSubsection(t *testing.T) {
	cfg := config.LLMConfig{
		Provider:       "anthropic",
		Endpoint:       "https://api.anthropic.com/v1",
		Model:          "claude-haiku-4-5-20251001",
		TimeoutSeconds: 10,
		FeedbackReview: config.FeedbackReviewConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled: true,
				APIKey:  "feature-key",
			},
		},
	}

	got, ok := welcomeSuggestionsProviderConfig(cfg)
	if !ok {
		t.Fatal("expected configured provider")
	}
	if got.APIKey != "feature-key" {
		t.Fatalf("APIKey = %q, want feature-key", got.APIKey)
	}
	if got.Provider != "anthropic" {
		t.Fatalf("Provider = %q, want anthropic", got.Provider)
	}
	if got.Endpoint != "https://api.anthropic.com/v1" {
		t.Fatalf("Endpoint = %q, want anthropic endpoint", got.Endpoint)
	}
	if got.Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("Model = %q, want shared model", got.Model)
	}
}

func TestWelcomeSuggestionsProviderConfigDoesNotReportUnconfiguredSubsection(t *testing.T) {
	cfg := config.LLMConfig{
		Provider: "anthropic",
		Endpoint: "https://api.anthropic.com/v1",
		FeedbackReview: config.FeedbackReviewConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled: true,
			},
		},
	}

	if got, ok := welcomeSuggestionsProviderConfig(cfg); ok {
		t.Fatalf("expected unconfigured provider, got %+v", got)
	}
}

func TestWelcomeSuggestionsProviderConfigSupportsGeminiADC(t *testing.T) {
	cfg := config.LLMConfig{
		Provider: "anthropic",
		Endpoint: "https://api.anthropic.com/v1",
		FeedbackReview: config.FeedbackReviewConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:  true,
				Provider: "gemini",
				Project:  "test-project",
				Region:   "global",
				Model:    "gemini-2.5-flash",
			},
		},
	}

	got, ok := welcomeSuggestionsProviderConfig(cfg)
	if !ok {
		t.Fatal("expected configured gemini provider")
	}
	if got.Provider != "gemini" {
		t.Fatalf("Provider = %q, want gemini", got.Provider)
	}
	if got.Endpoint != "" {
		t.Fatalf("Endpoint = %q, want empty so Gemini URL builder runs", got.Endpoint)
	}
}

func TestWelcomeSuggestionsProviderConfigClearsAnthropicDefaultForSharedGemini(t *testing.T) {
	cfg := config.LLMConfig{
		Provider: "gemini",
		Endpoint: "https://api.anthropic.com/v1",
		Project:  "test-project",
		Region:   "global",
		Model:    "gemini-2.5-flash",
	}

	got, ok := welcomeSuggestionsProviderConfig(cfg)
	if !ok {
		t.Fatal("expected configured gemini provider")
	}
	if got.Endpoint != "" {
		t.Fatalf("Endpoint = %q, want empty so Gemini URL builder runs", got.Endpoint)
	}
}
