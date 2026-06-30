package orggov

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestCanonicalizeModel(t *testing.T) {
	tests := []struct {
		name     string
		provider conversation.Provider
		model    string
		want     string
	}{
		{"anthropic prefix", conversation.ProviderAnthropic, "claude-opus-4-7", "anthropic/claude-opus-4-7"},
		{"openai prefix", conversation.ProviderOpenAI, "gpt-4o", "openai/gpt-4o"},
		{"google prefix", conversation.ProviderGoogle, "gemini-1.5-pro", "google/gemini-1.5-pro"},
		{"already qualified passes through", conversation.ProviderOpenAI, "azure/gpt-4o-2024-08", "azure/gpt-4o-2024-08"},
		{"google tuned model gets provider prefix", conversation.ProviderGoogle, "tunedModels/sales-v2-abc123", "google/tunedModels/sales-v2-abc123"},
		{"already-prefixed google tuned model passes through", conversation.ProviderGoogle, "google/tunedModels/sales-v2-abc123", "google/tunedModels/sales-v2-abc123"},
		{"empty model stays empty", conversation.ProviderOpenAI, "", ""},
		{"unknown provider preserves bare name", conversation.Provider("unknown"), "gpt-4o", "gpt-4o"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CanonicalizeModel(tc.provider, tc.model)
			if got != tc.want {
				t.Errorf("CanonicalizeModel(%v, %q) = %q, want %q", tc.provider, tc.model, got, tc.want)
			}
		})
	}
}
