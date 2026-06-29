package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/orggov"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// OrgSpendCapPolicy enforces per-org (and, via the cloud-side impl,
// per-team) spend caps. Runs before the upstream provider call so a
// hard-cap denial burns no quota and leaves no llm_cost row.
//
// The callback's warningLevel return is forwarded through the verdict
// audit params so the host can emit governance notifications even on
// allowed requests (80%/100% crossings on soft-mode caps).
type OrgSpendCapPolicy struct {
	callbacks     orggov.Callbacks
	orgIDForAgent func(ctx context.Context, agentID string) string
}

// NewOrgSpendCapPolicy constructs the policy. Nil callback or nil
// CheckSpendCap → no-op.
func NewOrgSpendCapPolicy(callbacks orggov.Callbacks, orgIDForAgent func(ctx context.Context, agentID string) string) *OrgSpendCapPolicy {
	return &OrgSpendCapPolicy{callbacks: callbacks, orgIDForAgent: orgIDForAgent}
}

func (OrgSpendCapPolicy) Name() string { return "org_spend_cap_policy" }

// Preprocess invokes the cloud callback. Block on hard-cap; otherwise
// always returns Allow but stamps audit_params with the warning level
// (consumed by the cloud emitter that fires spend.cap_warning_*).
func (p *OrgSpendCapPolicy) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if p == nil || p.callbacks.CheckSpendCap == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	orgID := ""
	if p.orgIDForAgent != nil {
		orgID = p.orgIDForAgent(ctx, req.AgentID())
	}
	if orgID == "" {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	allow, warningLevel, reason := p.callbacks.CheckSpendCap(ctx, orgID, req.AgentID())
	verdict := pipeline.RequestVerdict{
		Outcome:     pipeline.OutcomeAllow,
		AuditParams: map[string]any{},
	}
	if warningLevel != "" {
		verdict.AuditParams["spend_cap_warning_level"] = warningLevel
	}
	if !allow {
		if p.callbacks.RecordViolation != nil {
			p.callbacks.RecordViolation(ctx, orggov.ViolationEvent{
				OrgID:       orgID,
				UserID:      req.UserID(),
				AgentID:     req.AgentID(),
				PolicyKind:  "spend_cap",
				ActionTaken: "blocked",
				Detail:      reason,
			})
		}
		verdict.Outcome = pipeline.OutcomeDeny
		verdict.Reason = reason
		verdict.AuditParams["org_spend_cap_block"] = true
		verdict.AuditParams["reason"] = reason
		return verdict, nil
	}
	if reason != "" {
		// Soft-mode warning. Record as flagged (not blocked).
		if p.callbacks.RecordViolation != nil {
			p.callbacks.RecordViolation(ctx, orggov.ViolationEvent{
				OrgID:       orgID,
				UserID:      req.UserID(),
				AgentID:     req.AgentID(),
				PolicyKind:  "spend_cap",
				ActionTaken: "flagged",
				Detail:      reason,
			})
		}
		verdict.AuditParams["org_spend_cap_warning"] = reason
	}
	return verdict, nil
}
