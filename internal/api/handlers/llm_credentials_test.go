package handlers

import "testing"

// OpenAI provider must reject Anthropic-shaped keys (sk-ant-…). Without
// the explicit exclusion, the broader sk-* prefix swallows them and
// the wrong key ends up in the openai vault entry.
func TestValidateLLMAPIKey_OpenAIRejectsAnthropicShape(t *testing.T) {
	if reason, ok := validateLLMAPIKey("openai", "sk-ant-this-is-anthropic"); ok {
		t.Fatalf("expected sk-ant-… to be rejected for openai; got ok=%v", ok)
	} else if reason == "" {
		t.Fatalf("expected a rejection reason")
	}
}

func TestValidateLLMAPIKey_OpenAIAcceptsRealOpenAIKeys(t *testing.T) {
	for _, k := range []string{
		"sk-proj-realopenairealkey",
		"sk-realopenairealkey12345",
	} {
		if _, ok := validateLLMAPIKey("openai", k); !ok {
			t.Errorf("expected valid openai key %q to pass", k)
		}
	}
}

func TestValidateLLMAPIKey_AnthropicRejectsOpenAIKey(t *testing.T) {
	if _, ok := validateLLMAPIKey("anthropic", "sk-proj-realopenairealkey"); ok {
		t.Fatalf("expected sk-proj-… to be rejected for anthropic")
	}
}
