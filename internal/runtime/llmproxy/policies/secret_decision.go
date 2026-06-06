package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// SecretDecision handles the user's reply to a previously-held secret
// decision (allow_once, discard, vault, not_secret). Today this lives
// in the handler's maybeHandleLiteSecretDecision method, which has a
// dual-output shape: it can either short-circuit with a synthesized
// response OR rewrite the request body before letting it flow on.
//
// The policy captures both outputs via SecretDecisionResult:
//   - Handled=true → ShortCircuit with the synthesized response.
//   - Handled=false + ModifiedBody non-nil → Allow with ReplaceBody
//     (the user's decision rewrote the request; flow continues).
//   - Handled=false + ModifiedBody nil → Allow with no mutation.
type SecretDecision struct {
	resolver SecretDecisionResolver
}

// SecretDecisionResolver is the handler-supplied closure. Returns
// the full SecretDecisionResult including any rewritten body and the
// action string (allow_once / discard / vault / not_secret) that was applied.
type SecretDecisionResolver func(ctx context.Context, body []byte) SecretDecisionResult

// SecretDecisionResult is the full outcome of a secret-decision
// adjudication. Mirrors the legacy maybeHandleLiteSecretDecision's
// 4-tuple return.
type SecretDecisionResult struct {
	Handled      bool
	HTTPStatus   int
	Body         []byte
	ContentType  string
	Action       string
	ModifiedBody []byte
	Decision     string
	Outcome      string
	Reason       string
	AuditParams  map[string]any
}

// NewSecretDecision constructs the policy. nil resolver → Skip.
func NewSecretDecision(resolver SecretDecisionResolver) *SecretDecision {
	return &SecretDecision{resolver: resolver}
}

// Name returns the audit-friendly identifier.
func (SecretDecision) Name() string { return "secret_decision" }

// Preprocess dispatches to the resolver and translates the dual-output
// shape into a pipeline verdict.
func (p *SecretDecision) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if p.resolver == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	result := p.resolver(ctx, req.RawBody())

	fields := map[string]any{}
	if result.Action != "" {
		fields["secret_decision_action"] = string(result.Action)
	}
	if result.Decision != "" {
		fields["secret_decision_decision"] = result.Decision
	}
	if result.Outcome != "" {
		fields["secret_decision_outcome"] = result.Outcome
	}
	if result.Reason != "" {
		fields["secret_decision_reason"] = result.Reason
	}
	for k, v := range result.AuditParams {
		fields[k] = v
	}

	if result.Handled {
		status := result.HTTPStatus
		if status == 0 {
			status = 200
		}
		contentType := result.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		return pipeline.RequestVerdict{
			Outcome:     pipeline.OutcomeShortCircuit,
			AuditParams: fields,
			ShortCircuit: &pipeline.SyntheticResponse{
				Body:       result.Body,
				StatusCode: status,
				Headers:    map[string]string{"Content-Type": contentType},
			},
		}, nil
	}

	if result.ModifiedBody != nil {
		if err := mut.ReplaceBody(result.ModifiedBody); err != nil {
			return pipeline.RequestVerdict{}, err
		}
		return pipeline.RequestVerdict{
			Outcome:     pipeline.OutcomeAllow,
			AuditParams: fields,
		}, nil
	}

	return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow, AuditParams: fields}, nil
}

var _ pipeline.RequestPolicy = (*SecretDecision)(nil)
