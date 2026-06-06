package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/historystrip"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// SecretHistoryStrip removes proxy-injected secret-decision markers
// from conversation history before the upstream sees the request.
// Symmetric with SyntheticHistoryStrip but for secret prompts instead
// of approval prompts.
//
// The underlying transform is byte-fidelity tested
// (TestStripSecretDecisionHistoryPreservesThinkingBlockBytes) so
// migrating it is mechanical: same shape as the other strip policies.
// Pure body transformation; no state.
type SecretHistoryStrip struct{}

// NewSecretHistoryStrip constructs the policy.
func NewSecretHistoryStrip() *SecretHistoryStrip {
	return &SecretHistoryStrip{}
}

// Name returns the audit-friendly policy identifier.
func (SecretHistoryStrip) Name() string { return "secret_history_strip" }

// Preprocess runs the strip transform. Errors don't deny; stripping is
// a best-effort context cleanup.
func (p *SecretHistoryStrip) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	stripped, err := historystrip.StripSecretDecisionHistory(historystrip.SecretDecisionHistoryStripRequest{
		Provider: req.Provider(),
		Body:     req.RawBody(),
	})
	if err != nil {
		return pipeline.RequestVerdict{
			Outcome: pipeline.OutcomeSkip,
			Reason:  err.Error(),
			AuditParams: map[string]any{
				"secret_history_strip_error": err.Error(),
			},
		}, nil
	}
	if !stripped.Modified {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	if err := mut.ReplaceBody(stripped.Body); err != nil {
		return pipeline.RequestVerdict{}, err
	}
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeAllow,
		AuditParams: map[string]any{
			"secret_history_stripped": true,
		},
	}, nil
}

var _ pipeline.RequestPolicy = (*SecretHistoryStrip)(nil)
