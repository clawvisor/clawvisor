package orggov

import "github.com/clawvisor/clawvisor/internal/runtime/conversation"

// CanonicalizeModel returns the provider-qualified model identifier
// used by the cloud governance org_model_policy table. Bare names
// emitted by clients (e.g. "claude-opus-4-7", "gpt-4o") get prefixed
// with the upstream provider. Already-qualified identifiers (any
// string containing "/") pass through.
//
// Lives in this package, not in policies/, so the policy file itself
// stays provider-agnostic (enforced by
// TestProviderAbstraction_PoliciesDontMentionSpecificProviders).
func CanonicalizeModel(provider conversation.Provider, model string) string {
	if model == "" {
		return ""
	}
	for i := 0; i < len(model); i++ {
		if model[i] == '/' {
			return model
		}
	}
	prefix := providerPrefix(provider)
	if prefix == "" {
		return model
	}
	return prefix + "/" + model
}

func providerPrefix(p conversation.Provider) string {
	switch p {
	case conversation.ProviderAnthropic:
		return "anthropic"
	case conversation.ProviderOpenAI:
		return "openai"
	case conversation.ProviderGoogle:
		return "google"
	default:
		return ""
	}
}
