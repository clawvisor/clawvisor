package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// InspectorChain composes the inspector → boundary check sequence into
// a single ToolUseEvaluator. The verdict from inspector.Inspect flows
// to BoundaryCheck internally; the orchestrator sees one outcome per
// tool_use rather than two separate evaluator passes.
//
// Why composite instead of two pipeline evaluators: the inspector
// verdict needs to thread between the two steps. Today's
// newToolUseEvaluator does this with shared closure state. Modeling
// the chain as one ToolUseEvaluator preserves that information flow
// without introducing a per-tool-use state carrier in the pipeline.
//
// Outcomes:
//   - Inspector trigger miss → Skip (lets non-API tool_uses through to
//     whatever default-Allow path the orchestrator uses).
//   - Inspector says not an API call → Allow with verdict audit fields.
//   - Inspector ambiguous → Hold with per-tool HoldKey.
//   - Boundary check fails (verdict host not in placeholder allowlist)
//     → Deny with the reason in audit.
//   - Boundary check passes → Allow with full audit surface.
//
// Aggregates audit fields from both steps so downstream consumers see
// the inspection + boundary check result.
type InspectorChain struct {
	inspector       *inspector.Inspector
	allowedHostsFor AllowedHostsResolver
}

// NewInspectorChain composes the inspector + boundary check chain.
// Both dependencies are required; nil → degraded behavior (the
// composite emits Skip when inspector is nil, mirroring the legacy
// "no Inspector configured" gate).
func NewInspectorChain(insp *inspector.Inspector, resolver AllowedHostsResolver) *InspectorChain {
	return &InspectorChain{
		inspector:       insp,
		allowedHostsFor: resolver,
	}
}

// Name returns the audit-friendly evaluator identifier.
func (InspectorChain) Name() string { return "inspector_chain" }

// Evaluate runs the chain: inspect → resolve allowed hosts → boundary
// check. Emits one composite verdict.
func (c *InspectorChain) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if c.inspector == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	v := c.inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})

	fields := inspectorVerdictAuditFields(v)

	// Trigger miss: not an autovault-bearing call. Let downstream
	// default behavior decide.
	if v.Source == inspector.SourceTriggerMiss {
		return pipeline.ToolUseVerdict{
			Outcome:     pipeline.OutcomeSkip,
			AuditFields: fields,
		}, nil
	}

	// Ambiguous: fail closed with per-tool HoldKey.
	if v.Ambiguous {
		return pipeline.ToolUseVerdict{
			Outcome:     pipeline.OutcomeHold,
			Reason:      v.Reason,
			AuditFields: fields,
			HoldKey:     "ambiguous_" + tu.ID,
		}, nil
	}

	// Not an API call (per validator): allow through; nothing for
	// the boundary check to validate.
	if !v.IsAPICall {
		return pipeline.ToolUseVerdict{
			Outcome:     pipeline.OutcomeAllow,
			AuditFields: fields,
		}, nil
	}

	// Boundary check requires a resolver to look up the placeholder's
	// allowed hosts. Without one, can't enforce the boundary — fall back
	// to Allow with a marker so the absent enforcement is visible.
	if c.allowedHostsFor == nil {
		fields["boundary_check_skipped"] = "no_resolver"
		return pipeline.ToolUseVerdict{
			Outcome:     pipeline.OutcomeAllow,
			AuditFields: fields,
		}, nil
	}

	// Resolve allowed hosts for the first placeholder; the boundary
	// check uses this against the inspected host.
	var allowedHosts []string
	if len(v.Placeholders) > 0 {
		allowedHosts = c.allowedHostsFor(ctx, v.Placeholders[0])
	}

	ok, reason := inspector.BoundaryCheck(v, allowedHosts)
	fields["boundary_check_passed"] = ok
	if reason != "" {
		fields["boundary_check_reason"] = reason
	}
	if !ok {
		return pipeline.ToolUseVerdict{
			Outcome:     pipeline.OutcomeDeny,
			Reason:      reason,
			AuditFields: fields,
		}, nil
	}

	return pipeline.ToolUseVerdict{
		Outcome:     pipeline.OutcomeAllow,
		AuditFields: fields,
	}, nil
}

// inspectorVerdictAuditFields builds the audit-field map carrying the
// inspector verdict's surface. Extracted so InspectorEvaluator and
// InspectorChain can produce identical audit shapes.
func inspectorVerdictAuditFields(v inspector.Verdict) map[string]any {
	fields := map[string]any{
		"inspector_source": string(v.Source),
		"inspector_is_api": v.IsAPICall,
	}
	if v.Reason != "" {
		fields["inspector_reason"] = v.Reason
	}
	if v.Method != "" {
		fields["inspector_method"] = v.Method
	}
	if v.Host != "" {
		fields["inspector_host"] = v.Host
	}
	if v.Path != "" {
		fields["inspector_path"] = v.Path
	}
	if len(v.Placeholders) > 0 {
		fields["inspector_placeholders"] = v.Placeholders
	}
	return fields
}

var _ pipeline.ToolUseEvaluator = (*InspectorChain)(nil)
