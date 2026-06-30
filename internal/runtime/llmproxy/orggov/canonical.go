package orggov

import (
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// CanonicalizeModel returns the provider-qualified model identifier
// used by the cloud governance org_model_policy table. Bare names
// emitted by clients (e.g. "claude-opus-4-7", "gpt-4o") get prefixed
// with the upstream provider. Already-qualified identifiers (any
// string containing "/", e.g. "google/gemini-1.5-pro" or
// "azure/gpt-4o-2024-08") pass through unchanged.
//
// One narrow exception: resource-prefixed identifiers (currently just
// Gemini's "tunedModels/<id>") look like they contain "/" but aren't
// provider qualifiers — they're a model-class scope inside a single
// provider. Those still get the provider prefix prepended, producing
// e.g. "google/tunedModels/<id>". Without this, base + tuned models
// with the same trailing name would collide under one policy entry.
//
// Lives in this package, not in policies/, so the policy file itself
// stays provider-agnostic (enforced by
// TestProviderAbstraction_PoliciesDontMentionSpecificProviders).
func CanonicalizeModel(provider conversation.Provider, model string) string {
	if model == "" {
		return ""
	}
	prefix := providerPrefix(provider)
	if prefix == "" {
		// Provider unknown — pass through. Same disposition as the
		// pre-existing "fall back to whatever the caller said" path.
		return model
	}
	if needsProviderPrefix(model) {
		return prefix + "/" + model
	}
	return model
}

// needsProviderPrefix returns true when model needs the provider
// prepended. Bare names need it. Resource-scoped identifiers like
// "tunedModels/<id>" also need it — they look qualified but aren't.
// Anything else containing "/" is treated as already provider-qualified
// (e.g. "openai/gpt-4o", "azure/gpt-4o-2024-08") and passes through.
func needsProviderPrefix(model string) bool {
	if !strings.ContainsRune(model, '/') {
		return true
	}
	// Known resource-scope prefixes that share a provider:
	for _, p := range []string{"tunedModels/"} {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
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
