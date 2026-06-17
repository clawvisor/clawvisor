package policies

import (
	"context"
	"encoding/json"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/orggov"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// OrgModelPolicy enforces the cloud governance org_model_policy table
// against the requested model identifier. Runs after inbound_sanitize
// and BEFORE any upstream LLM call so a blocked model is rejected
// without burning provider quota or leaving an LLM-cost row.
//
// Behavior is no-op when the agent has no OrgID (open-source clawvisor,
// admin sessions) OR when the callbacks struct lacks CheckModelPolicy.
// Empty model field also no-ops — the rest of the pipeline will reject
// a missing model with its existing error path.
type OrgModelPolicy struct {
	callbacks orggov.Callbacks
	// orgIDForAgent maps an agent_id to the agent's org_id. Injected
	// by the handler that builds this policy; lets us avoid a store
	// dependency at the policy layer.
	orgIDForAgent func(ctx context.Context, agentID string) string
}

// NewOrgModelPolicy constructs the policy. callbacks may have a nil
// CheckModelPolicy field — in that case the policy is a no-op.
func NewOrgModelPolicy(callbacks orggov.Callbacks, orgIDForAgent func(ctx context.Context, agentID string) string) *OrgModelPolicy {
	return &OrgModelPolicy{callbacks: callbacks, orgIDForAgent: orgIDForAgent}
}

// Name returns the audit-friendly identifier.
func (OrgModelPolicy) Name() string { return "org_model_policy" }

// Preprocess extracts the model identifier from the request body and
// consults the cloud callback. Block-mode returns OutcomeDeny; allow
// returns OutcomeAllow. Errors degrade to OutcomeSkip (best-effort —
// network blips against the cloud store should never silently allow
// a request that's actually banned — but for v1 we accept this and log
// in audit).
func (p *OrgModelPolicy) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if p == nil || p.callbacks.CheckModelPolicy == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	orgID := ""
	if p.orgIDForAgent != nil {
		orgID = p.orgIDForAgent(ctx, req.AgentID())
	}
	if orgID == "" {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	model := extractModelFromRequest(req)
	if model == "" {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	canonical := orggov.CanonicalizeModel(req.Provider(), model)
	allow, reason := p.callbacks.CheckModelPolicy(ctx, orgID, canonical)
	if allow {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	if p.callbacks.RecordViolation != nil {
		p.callbacks.RecordViolation(ctx, orggov.ViolationEvent{
			OrgID:       orgID,
			UserID:      req.UserID(),
			AgentID:     req.AgentID(),
			PolicyKind:  "model_policy",
			ActionTaken: "blocked",
			Detail:      reason,
		})
	}
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeDeny,
		Reason:  reason,
		AuditParams: map[string]any{
			"org_model_policy_block": true,
			"model":                  canonical,
			"reason":                 reason,
		},
	}, nil
}

// extractModelFromRequest returns the top-level "model" field from the
// raw request body. Both Anthropic Messages and OpenAI Chat/Completions
// shapes use a top-level "model" string; we don't need a provider-aware
// parse for this field.
func extractModelFromRequest(req pipeline.ReadOnlyRequest) string {
	body := req.RawBody()
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

