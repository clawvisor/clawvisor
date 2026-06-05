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
	triggerMissAuth TriggerMissAuthorizer
}

// TriggerMissAuthorizer authorizes a tool_use that the inspector
// classified as "trigger-miss" — no autovault placeholder, no
// credential mediation needed. The handler implements this to run
// runtimedecision.EvaluateAuthorization plus the readonly-shell /
// sensitive-path special cases, returning the resulting pipeline
// verdict. When nil, InspectorChain returns Skip on trigger-miss
// (leaves the decision to downstream evaluators or the default-Allow
// fallback).
type TriggerMissAuthorizer func(ctx context.Context, tu conversation.ToolUse, mut pipeline.ToolUseMutator) pipeline.ToolUseVerdict

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

// WithTriggerMissAuthorizer returns the same chain with the trigger-miss
// authorization branch enabled. Without this, the chain returns Skip
// on trigger-miss; with it, the chain calls the authorizer and returns
// its verdict for the trigger-miss path.
func (c *InspectorChain) WithTriggerMissAuthorizer(auth TriggerMissAuthorizer) *InspectorChain {
	c.triggerMissAuth = auth
	return c
}

// Name returns the audit-friendly evaluator identifier.
func (InspectorChain) Name() string { return "inspector_chain" }

// Evaluate runs the chain: inspect → resolve allowed hosts → boundary
// check. Emits one composite verdict.
func (c *InspectorChain) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, mut pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if c.inspector == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	v := c.inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})

	// Stub-placeholder downgrade: if the only `autovault_…` substrings
	// in this tool_use are too short to be real vault references —
	// test fixtures, prose examples, doc snippets — there's no
	// credential to mediate. Downgrade to trigger-miss so the
	// surrounding tool call (often an Edit of a test file that mentions
	// the literal) is evaluated under normal authorization rather than
	// refused as ambiguous.
	if v.Source != inspector.SourceTriggerMiss && inspector.AllPlaceholdersAreStubs(v.Placeholders) {
		v = inspector.Verdict{
			IsAPICall: false,
			Source:    inspector.SourceTriggerMiss,
			Reason:    "placeholders are stub-length (no real vault reference)",
		}
	}

	fields := inspectorVerdictAuditFields(v)

	// Trigger miss: not an autovault-bearing call. If a trigger-miss
	// authorizer is configured, delegate to it (runs EvaluateAuthorization
	// + readonly-shell / sensitive-path branches). Otherwise Skip and let
	// downstream evaluators / default-Allow handle it.
	if v.Source == inspector.SourceTriggerMiss {
		if c.triggerMissAuth != nil {
			verdict := c.triggerMissAuth(ctx, tu, mut)
			// Merge inspector audit fields with the authorizer's so the
			// row carries both surfaces.
			if verdict.AuditFields == nil {
				verdict.AuditFields = fields
			} else {
				for k, val := range fields {
					if _, exists := verdict.AuditFields[k]; !exists {
						verdict.AuditFields[k] = val
					}
				}
			}
			return verdict, nil
		}
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

	// Credentialed API call. Boundary check decides whether to fail
	// closed; Allow paths return Skip so downstream stages
	// (TaskScopeEvaluator + IntentVerifyEvaluator + CredentialRewriteEvaluator)
	// can run the credentialed authorization + rewrite flow.
	if c.allowedHostsFor == nil {
		// Without a resolver we can't enforce boundary, but the call is
		// credentialed — let downstream rewrite the tool_use. Marking
		// the audit field documents the gap.
		fields["boundary_check_skipped"] = "no_resolver"
		return pipeline.ToolUseVerdict{
			Outcome:     pipeline.OutcomeSkip,
			AuditFields: fields,
		}, nil
	}

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

	// Boundary passed — let downstream stages handle credentialed
	// authorization + rewrite.
	return pipeline.ToolUseVerdict{
		Outcome:     pipeline.OutcomeSkip,
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
