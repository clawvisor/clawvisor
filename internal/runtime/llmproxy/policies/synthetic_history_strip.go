package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// SyntheticHistoryStrip removes proxy-injected approval prompts (and
// their bare "approve"/"deny" replies) from conversation history
// before the upstream sees the request.
//
// Pure body transformation; no state, no side effects beyond the body
// swap and the
// `synthetic_approval_history_stripped` audit flag.
//
// Unlike anthropic_sanitize, this policy runs for both providers —
// the underlying helper dispatches per-provider internally.
type SyntheticHistoryStrip struct{}

// NewSyntheticHistoryStrip constructs the policy. No dependencies.
func NewSyntheticHistoryStrip() *SyntheticHistoryStrip {
	return &SyntheticHistoryStrip{}
}

// Name returns the audit-friendly policy identifier.
func (SyntheticHistoryStrip) Name() string { return "synthetic_history_strip" }

// Preprocess runs the strip transform. Unlike anthropic_sanitize, an
// error here is not fatal. The policy returns OutcomeSkip with the
// error in Reason so the orchestrator can surface it to logging without
// denying the request.
func (p *SyntheticHistoryStrip) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	stripped, err := llmproxy.StripSyntheticApprovalHistory(llmproxy.SyntheticApprovalHistoryStripRequest{
		Provider: req.Provider(),
		Body:     req.RawBody(),
	})
	if err != nil {
		// Best-effort context cleanup: return Skip with the error
		// tagged in audit.
		return pipeline.RequestVerdict{
			Outcome: pipeline.OutcomeSkip,
			Reason:  err.Error(),
			AuditParams: map[string]any{
				"synthetic_history_strip_error": err.Error(),
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
			"synthetic_approval_history_stripped": true,
		},
	}, nil
}

var _ pipeline.RequestPolicy = (*SyntheticHistoryStrip)(nil)
