package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// AnthropicSanitize drops empty text blocks from Anthropic Messages
// requests that would otherwise trip the upstream's stricter content
// validation. Pure body transformation — no state, no side effects,
// no audit beyond the `anthropic_empty_text_sanitized` flag.
//
// The underlying transform is llmproxy.SanitizeAnthropicRequest, which
// has been byte-fidelity tested for thinking-block preservation since
// before the refactor; this policy is a thin pipeline wrapper around it.
type AnthropicSanitize struct{}

// NewAnthropicSanitize constructs the policy. No dependencies needed.
func NewAnthropicSanitize() *AnthropicSanitize {
	return &AnthropicSanitize{}
}

// Name returns the audit-friendly policy identifier.
func (AnthropicSanitize) Name() string { return "anthropic_sanitize" }

// Preprocess runs the sanitizer iff the request targets Anthropic.
// Non-Anthropic providers get OutcomeSkip with no mutations queued.
//
// On a Anthropic parse failure, the policy returns Outcome=Deny with
// Reason set to the underlying error — the orchestrator turns that
// into a 400 MALFORMED_REQUEST in the handler. (Today the handler
// inlines this check; the refactor relocates the decision but
// preserves the response shape via the deny path.)
func (p *AnthropicSanitize) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if req.Provider() != conversation.ProviderAnthropic {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	sanitizedBody, sanitized, err := llmproxy.SanitizeAnthropicRequest(req.RawBody())
	if err != nil {
		return pipeline.RequestVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  err.Error(),
			AuditFields: map[string]any{
				"deny_outcome": "malformed_request",
			},
		}, nil
	}
	if !sanitized {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}

	if err := mut.ReplaceBody(sanitizedBody); err != nil {
		return pipeline.RequestVerdict{}, err
	}
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeAllow,
		AuditFields: map[string]any{
			"anthropic_empty_text_sanitized": true,
		},
	}, nil
}

// Compile-time assertion: AnthropicSanitize satisfies RequestPolicy.
var _ pipeline.RequestPolicy = (*AnthropicSanitize)(nil)
