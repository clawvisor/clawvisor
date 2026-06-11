package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/historystrip"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// SyntheticHistoryStrip removes proxy-injected approval prompts (and
// their bare "approve"/"deny" replies) from conversation history
// before the upstream sees the request.
//
// Unlike anthropic_sanitize, this policy runs for both providers —
// the underlying helper dispatches per-provider internally.
//
// Side effects:
//   - Body swap (the policy's reason to exist).
//   - `synthetic_approval_history_stripped` audit flag.
//   - When LookupBuilder is wired (production path), one read query
//     to the lifecycle audit per substituted-prompt match in the
//     body. The query is best-effort — a store outage degrades
//     gracefully to the legacy drop-the-turn behavior, the strip
//     never blocks on it past its own bounded ctx.
//
// LookupBuilder, when non-nil, is called per-request to produce a
// historystrip.ReconstructionLookup bound to that request's
// context. When the strip path finds a substituted-prompt assistant
// turn, the lookup queries the lifecycle audit and (on hit) the
// turn is REPLACED with a synthetic [tool_use(original), tool_result]
// pair instead of dropped — the model sees its own call in
// history on every subsequent turn, not just the post-approval
// one. nil keeps the legacy drop-the-turn behavior (acceptable for
// tests / installs without persistence wired).
type SyntheticHistoryStrip struct {
	LookupBuilder func(ctx context.Context) historystrip.ReconstructionLookup
}

// NewSyntheticHistoryStrip constructs the policy with no lookup —
// strip behaves as a pure drop. Backward-compatible entry point.
func NewSyntheticHistoryStrip() *SyntheticHistoryStrip {
	return &SyntheticHistoryStrip{}
}

// NewSyntheticHistoryStripWithLookup constructs the policy with a
// per-request lookup builder. The builder takes the request
// context (so the lookup can pass it to store calls) and returns
// the actual lookup closure.
func NewSyntheticHistoryStripWithLookup(builder func(ctx context.Context) historystrip.ReconstructionLookup) *SyntheticHistoryStrip {
	return &SyntheticHistoryStrip{LookupBuilder: builder}
}

// Name returns the audit-friendly policy identifier.
func (SyntheticHistoryStrip) Name() string { return "synthetic_history_strip" }

// Preprocess runs the strip transform. Unlike anthropic_sanitize, an
// error here is not fatal. The policy returns OutcomeSkip with the
// error in Reason so the orchestrator can surface it to logging without
// denying the request.
func (p *SyntheticHistoryStrip) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	var lookup historystrip.ReconstructionLookup
	if p.LookupBuilder != nil {
		lookup = p.LookupBuilder(ctx)
	}
	stripped, err := historystrip.StripSyntheticApprovalHistory(historystrip.SyntheticApprovalHistoryStripRequest{
		Provider:             req.Provider(),
		Body:                 req.RawBody(),
		ReconstructionLookup: lookup,
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
