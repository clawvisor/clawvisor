package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// SecretHold wraps the inbound-secret hold check that gates the
// request when a previously-undecided secret needs user adjudication.
// Today this lives in the handler's maybeHoldInboundSecret method,
// which writes the hold-prompt response to w directly.
//
// To make this a policy, the handler exposes a SecretHoldResolver
// closure: when the resolver returns held=true, the policy emits
// ShortCircuit with the synthesized hold-prompt body. The handler
// receives the ShortCircuit and writes the response.
//
// Outcomes:
//   - nil resolver → Skip (no hold infrastructure configured)
//   - resolver returns held=false → Allow with no mutation
//   - resolver returns held=true → ShortCircuit with the synthesized
//     hold-prompt response carrying status + body + content type
type SecretHold struct {
	resolver SecretHoldResolver
}

// SecretHoldResolver is the handler-supplied closure that runs the
// hold check. Returns SecretHoldResult; Held=true indicates the
// request was held and a hold-prompt response should be returned to
// the client.
type SecretHoldResolver func(ctx context.Context, body []byte) SecretHoldResult

// SecretHoldResult is what the resolver returns. When Held=true the
// policy short-circuits with the supplied body/status/contentType.
type SecretHoldResult struct {
	Held        bool
	HTTPStatus  int
	Body        []byte
	ContentType string
	Decision    string
	Outcome     string
	Reason      string
	// AuditFields are additional audit-row entries the hold check
	// produced (e.g., the pending secret ID).
	AuditFields map[string]any
}

// NewSecretHold constructs the policy. nil resolver → Skip.
func NewSecretHold(resolver SecretHoldResolver) *SecretHold {
	return &SecretHold{resolver: resolver}
}

// Name returns the audit-friendly identifier.
func (SecretHold) Name() string { return "secret_hold" }

// Preprocess invokes the hold resolver. Held=true → ShortCircuit;
// Held=false → Allow.
func (p *SecretHold) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if p.resolver == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	result := p.resolver(ctx, req.RawBody())
	if !result.Held {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}

	status := result.HTTPStatus
	if status == 0 {
		status = 200
	}
	contentType := result.ContentType
	if contentType == "" {
		contentType = "application/json"
	}

	fields := map[string]any{
		"secret_hold_held": true,
	}
	if result.Decision != "" {
		fields["secret_hold_decision"] = result.Decision
	}
	if result.Outcome != "" {
		fields["secret_hold_outcome"] = result.Outcome
	}
	if result.Reason != "" {
		fields["secret_hold_reason"] = result.Reason
	}
	for k, v := range result.AuditFields {
		fields[k] = v
	}

	return pipeline.RequestVerdict{
		Outcome:     pipeline.OutcomeShortCircuit,
		AuditFields: fields,
		ShortCircuit: &pipeline.SyntheticResponse{
			Body:       result.Body,
			StatusCode: status,
			Headers:    map[string]string{"Content-Type": contentType},
		},
	}, nil
}

var _ pipeline.RequestPolicy = (*SecretHold)(nil)
