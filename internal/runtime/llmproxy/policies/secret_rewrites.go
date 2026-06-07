package policies

import (
	"context"
	"fmt"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// SecretRewrites applies remembered secret-redaction rewrites to the
// inbound request body. Today this is one of three sub-steps inside
// the handler's preprocessLiteSecretBody method. The other two
// (StripSecretDecisionHistory — already migrated as
// SecretHistoryStrip — and maybeHoldInboundSecret — handler-coupled
// response writer, remains inline) follow the same pattern.
//
// The policy delegates to a SecretRewritesResolver closure that the
// handler bakes (agent identity + vault interactions) into. Keeping
// it as a closure lets the policy stay decoupled from the
// h.applyRememberedSecretRewrites method's identity/store
// dependencies.
//
// Outcomes:
//   - resolver returns error → pipeline error; caller must fail closed.
//   - resolver returns modified=false → Allow with no mutation.
//   - resolver returns modified=true → Allow with ReplaceBody +
//     `secret_rewrites_applied:true` audit flag.
type SecretRewrites struct {
	resolver SecretRewritesResolver
}

// SecretRewritesResolver returns the rewritten body and whether any
// rewrites applied. The handler closes over (agent, requestID,
// provider) at construction time. Errors indicate the resolver could
// not determine whether remembered secrets were present and should fail
// closed rather than forwarding the original body.
type SecretRewritesResolver func(ctx context.Context, body []byte) (rewritten []byte, modified bool, err error)

// NewSecretRewrites constructs the policy. nil resolver → Skip.
func NewSecretRewrites(resolver SecretRewritesResolver) *SecretRewrites {
	return &SecretRewrites{resolver: resolver}
}

// Name returns the audit-friendly identifier.
func (SecretRewrites) Name() string { return "secret_rewrites" }

// Preprocess dispatches to the resolver. On modified=true, queues
// ReplaceBody and sets the audit flag.
func (p *SecretRewrites) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if p.resolver == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	rewritten, modified, err := p.resolver(ctx, req.RawBody())
	if err != nil {
		return pipeline.RequestVerdict{}, fmt.Errorf("secret rewrites: %w", err)
	}
	if !modified {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	if err := mut.ReplaceBody(rewritten); err != nil {
		return pipeline.RequestVerdict{}, err
	}
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeAllow,
		AuditParams: map[string]any{
			"secret_rewrites_applied": true,
		},
	}, nil
}

var _ pipeline.RequestPolicy = (*SecretRewrites)(nil)
