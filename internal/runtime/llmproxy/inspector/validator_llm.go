package inspector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// LLMClientValidator is a provider-agnostic Validator that delegates
// classification to whichever LLM the daemon is configured to use for
// verification (Gemini / Anthropic / OpenAI / Vertex). It mirrors the
// pattern the IntentVerifier uses: read live VerificationConfig per
// call, build a fresh llm.Client, send (system + user) messages, and
// parse the model's JSON response.
//
// Construct via NewLLMClientValidator. The ConfigFn returns the
// current verification config so live edits or env-var overrides flow
// through without restarting the daemon.
//
// When the verification config has Enabled=false or no API key /
// project, Validate returns ambiguous=true with a "validator
// disabled" reason — same shape as AmbiguousValidator. The rewriter
// fails closed.
type LLMClientValidator struct {
	// ConfigFn returns the current verification config. Required.
	ConfigFn func() config.VerificationConfig
	// Logger is optional; defaults to slog.Default().
	Logger *slog.Logger
	// PromptOverride lets tests inject a deterministic prompt. Empty
	// uses the package-level ValidatorPrompt.
	PromptOverride string
}

// NewLLMClientValidator returns a validator backed by the daemon's
// configured LLM provider. Pass health.VerificationConfig directly.
func NewLLMClientValidator(configFn func() config.VerificationConfig, logger *slog.Logger) *LLMClientValidator {
	if logger == nil {
		logger = slog.Default()
	}
	return &LLMClientValidator{ConfigFn: configFn, Logger: logger}
}

// Validate implements Validator. Returns ambiguous=true (rather than an
// error) on any failure so the rewriter fails closed without surfacing
// a low-signal exception.
func (v *LLMClientValidator) Validate(ctx context.Context, t ToolUse) (Verdict, error) {
	if v == nil || v.ConfigFn == nil {
		return Verdict{Ambiguous: true, Reason: "validator not configured"}, nil
	}
	cfg := v.ConfigFn()
	if !cfg.Enabled {
		return Verdict{Ambiguous: true, Reason: "validator disabled in config"}, nil
	}
	if !llmProviderConfigured(cfg.LLMProviderConfig) {
		return Verdict{Ambiguous: true, Reason: "validator provider not configured (no api_key/project)"}, nil
	}

	prompt := v.PromptOverride
	if prompt == "" {
		prompt = ValidatorPrompt
	}

	client := llm.NewClient(cfg.LLMProviderConfig)
	messages := []llm.ChatMessage{
		{Role: "system", Content: prompt, CacheControl: true},
		{Role: "user", Content: validatorUserMessage(t)},
	}

	raw, _, err := client.CompleteWithUsage(ctx, messages)
	if err != nil {
		v.Logger.WarnContext(ctx, "lite-proxy: validator LLM call failed",
			"tool", t.Name, "err", err.Error())
		return Verdict{Ambiguous: true, Reason: "validator transport error"}, nil
	}

	verdict, err := parseValidatorJSON(raw)
	if err != nil {
		v.Logger.WarnContext(ctx, "lite-proxy: validator response parse failed",
			"tool", t.Name, "err", err.Error())
		return Verdict{Ambiguous: true, Reason: "validator parse error"}, nil
	}
	return verdict, nil
}

// llmProviderConfigured reports whether the provider config carries
// enough material for llm.NewClient to actually reach a provider.
// Different providers require different credentials: API-key providers
// need a key; Vertex/Gemini-on-Vertex authenticates via ADC and only
// needs Project+Region.
func llmProviderConfigured(cfg config.LLMProviderConfig) bool {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch provider {
	case "gemini", "vertex":
		return strings.TrimSpace(cfg.Project) != ""
	default:
		return strings.TrimSpace(cfg.APIKey) != ""
	}
}

// validatorUserMessage constructs the user-role message the validator
// LLM sees. The shape matches the AnthropicValidator user message so
// we can swap them out without re-tuning the prompt.
func validatorUserMessage(t ToolUse) string {
	return fmt.Sprintf("tool_name: %q\ntool_input: %s", t.Name, string(t.Input))
}

// parseValidatorJSON pulls the JSON verdict from a free-form model
// response, tolerating leading/trailing whitespace and the common
// ```json fence shape.
func parseValidatorJSON(raw string) (Verdict, error) {
	text := strings.TrimSpace(raw)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)
	// Locate the outer JSON object — some providers preface with a
	// note like "Here's the JSON:" even when told not to.
	if i := strings.IndexByte(text, '{'); i > 0 {
		text = text[i:]
	}
	if j := strings.LastIndexByte(text, '}'); j >= 0 && j < len(text)-1 {
		text = text[:j+1]
	}
	var parsed anthropicValidatorResponse
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return Verdict{}, fmt.Errorf("validator JSON: %w", err)
	}
	// Mirror the AnthropicValidator: if the model claims is_api_call
	// but didn't specify a method, force ambiguous=true so the
	// canonicalMethod default ("GET") doesn't silently authorize a
	// method the validator never asserted.
	ambiguous := parsed.Ambiguous
	method := strings.TrimSpace(parsed.Method)
	if parsed.IsAPICall && method == "" {
		ambiguous = true
	}
	return Verdict{
		IsAPICall:           parsed.IsAPICall,
		Ambiguous:           ambiguous,
		Method:              canonicalMethodOrEmpty(method),
		Host:                strings.ToLower(strings.TrimSpace(parsed.Host)),
		Path:                parsed.Path,
		CredentialLocations: parsed.CredentialLocations,
		Reason:              parsed.Reason,
	}, nil
}
