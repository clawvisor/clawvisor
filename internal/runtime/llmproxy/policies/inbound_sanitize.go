package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// InboundSanitize reverts proxy-injected transport details
// (cv-nonce-* tokens, X-Clawvisor-* headers, rewritten URLs, daemon
// URLs) from assistant tool_use blocks in conversation history before
// the upstream model sees them.
//
// Without this, models pattern-match from their own history and start
// emitting proxy artifacts verbatim on subsequent turns, bypassing
// the rewrite path entirely. The current handler treats sanitize
// failures as best-effort logging — preserved here as OutcomeSkip
// with the error tagged in audit.
//
// First policy to take dependencies via constructor: ResolverBaseURL
// and ControlBaseURL come from the handler's configuration, not the
// request. This is the pattern future policies with deps will follow
// (e.g., secret_detection takes the suppression cache, control_notice
// takes the policy loader).
type InboundSanitize struct {
	resolverBaseURL string
	controlBaseURL  string
}

// NewInboundSanitize constructs the policy with its URL dependencies.
// Both URLs come from LLMEndpointHandler configuration today.
func NewInboundSanitize(resolverBaseURL, controlBaseURL string) *InboundSanitize {
	return &InboundSanitize{
		resolverBaseURL: resolverBaseURL,
		controlBaseURL:  controlBaseURL,
	}
}

// Name returns the audit-friendly policy identifier.
func (InboundSanitize) Name() string { return "inbound_sanitize" }

// Preprocess invokes llmproxy.SanitizeInboundHistory with the policy's
// configured URLs. Errors don't deny — best-effort semantic matches
// the legacy handler.
func (p *InboundSanitize) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	sanitized, err := llmproxy.SanitizeInboundHistory(llmproxy.SanitizeInboundRequest{
		Provider:        req.Provider(),
		Body:            req.RawBody(),
		ResolverBaseURL: p.resolverBaseURL,
		ControlBaseURL:  p.controlBaseURL,
	})
	if err != nil {
		return pipeline.RequestVerdict{
			Outcome: pipeline.OutcomeSkip,
			Reason:  err.Error(),
			AuditParams: map[string]any{
				"inbound_sanitize_error": err.Error(),
			},
		}, nil
	}
	if !sanitized.Modified {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	if err := mut.ReplaceBody(sanitized.Body); err != nil {
		return pipeline.RequestVerdict{}, err
	}
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeAllow,
		AuditParams: map[string]any{
			"inbound_history_sanitized": true,
		},
	}, nil
}

var _ pipeline.RequestPolicy = (*InboundSanitize)(nil)
