package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// BoundaryCheckEvaluator validates an inspector verdict against the
// placeholder's bound-service host allowlist. Runs after the
// InspectorEvaluator — reads its verdict from AuditFields (set by
// InspectorEvaluator.Evaluate).
//
// The bound-service host allowlist is per-placeholder; the lookup
// happens via the AllowedHostsResolver callback so this evaluator
// doesn't import the placeholder store directly.
//
// Outcomes:
//   - Tool_use isn't an API call (per inspector) → Skip (let downstream
//     evaluators decide; the legacy code doesn't deny non-API tool_uses
//     here, it just lets them through).
//   - Boundary check fails → Deny per-tool (the placeholder's host
//     allowlist intentionally rejected this target).
//   - Boundary check passes → Allow.
type BoundaryCheckEvaluator struct {
	allowedHostsFor AllowedHostsResolver
}

// AllowedHostsResolver maps a placeholder string to the set of hosts
// that placeholder's bound service is authorized to forward to.
// Callers wire this from the placeholder store.
//
// Deprecated: prefer BoundaryResolver, which returns typed denial
// reasons that distinguish placeholder-unknown / ownership-mismatch /
// host-not-allowed instead of compressing all three into a binary.
// The chain still accepts AllowedHostsResolver for backward compat
// and adapts via boundaryResolverFromHosts.
type AllowedHostsResolver func(ctx context.Context, placeholder string) []string

// BoundaryResolver evaluates the credentialed boundary check for a
// given inspector verdict. The resolver runs the three discrete
// failure-mode checks (placeholder exists, ownership matches, host in
// allowlist) and returns a typed decision.
//
// Returning Allowed=true + AllowedHosts=nil means "no placeholder
// supplied, skip boundary check" (the inspector verdict didn't carry
// one); the chain treats this as no-op-Skip.
type BoundaryResolver func(ctx context.Context, v inspector.Verdict) BoundaryDecision

// BoundaryDecision is the typed outcome of a BoundaryResolver call.
// When !Allowed, DenyReason names the specific failure mode so audit
// rows distinguish the three cases.
type BoundaryDecision struct {
	Allowed      bool
	DenyReason   pipeline.BoundaryDenyReason
	Reason       string // human-readable; pairs with DenyReason
	AllowedHosts []string
}

// boundaryResolverFromHosts adapts a legacy AllowedHostsResolver into
// the typed BoundaryResolver. Compresses every failure mode to
// BoundaryDenyReasonHostNotAllowed; callers that want the discrete
// reasons should wire a BoundaryResolver directly.
func boundaryResolverFromHosts(legacy AllowedHostsResolver) BoundaryResolver {
	if legacy == nil {
		return nil
	}
	return func(ctx context.Context, v inspector.Verdict) BoundaryDecision {
		var placeholder string
		if len(v.Placeholders) > 0 {
			placeholder = v.Placeholders[0]
		}
		hosts := legacy(ctx, placeholder)
		ok, reason := inspector.BoundaryCheck(v, hosts)
		decision := BoundaryDecision{Allowed: ok, Reason: reason, AllowedHosts: hosts}
		if !ok {
			decision.DenyReason = pipeline.BoundaryDenyReasonHostNotAllowed
		}
		return decision
	}
}

// NewBoundaryCheckEvaluator constructs the evaluator. nil resolver
// → Skip on every tool_use (matches "no boundary-check infrastructure
// configured").
func NewBoundaryCheckEvaluator(resolver AllowedHostsResolver) *BoundaryCheckEvaluator {
	return &BoundaryCheckEvaluator{allowedHostsFor: resolver}
}

// Name returns the audit-friendly evaluator identifier.
func (BoundaryCheckEvaluator) Name() string { return "boundary_check" }

// Evaluate runs the boundary check. Reads inspector verdict from
// AuditFields (set by InspectorEvaluator); if those fields aren't
// present, this evaluator can't proceed and returns Skip.
//
// Reconstructs a minimal inspector.Verdict from AuditFields rather
// than passing the verdict struct in a per-evaluator carrier. The
// AuditFields approach has the property that audit consumers see
// the inspection result for free, and evaluators are coupled only
// to the AuditFields contract.
func (e *BoundaryCheckEvaluator) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if e.allowedHostsFor == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	// In the current chain composition this evaluator runs AFTER
	// InspectorEvaluator. The orchestrator's first-non-Skip-wins rule
	// means BoundaryCheckEvaluator only runs when InspectorEvaluator
	// returned Skip (trigger miss or nil inspector). In that case
	// there's no verdict to boundary-check; return Skip.
	//
	// Future wiring: extract the inspector verdict from an explicit
	// carrier rather than reading AuditFields. For now this gate is
	// the right default — boundary checks shouldn't run without an
	// inspector verdict.
	return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
}

// EvaluateWithVerdict is the variant that takes an explicit inspector
// verdict. Used by the composite InspectorChain (below) that runs
// inspector + boundary in one evaluator pass rather than as two
// separate orchestrator passes.
func (e *BoundaryCheckEvaluator) EvaluateWithVerdict(_ context.Context, v inspector.Verdict, allowedHosts []string) pipeline.ToolUseVerdict {
	if e.allowedHostsFor == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}
	}
	ok, reason := inspector.BoundaryCheck(v, allowedHosts)
	fields := map[string]any{
		"boundary_check_passed": ok,
	}
	if reason != "" {
		fields["boundary_check_reason"] = reason
	}
	placeholder := ""
	if len(v.Placeholders) > 0 {
		placeholder = v.Placeholders[0]
	}
	fact := pipeline.BoundaryFact{
		Passed:      ok,
		Reason:      reason,
		Placeholder: placeholder,
		Host:        v.Host,
	}
	if ok {
		return pipeline.ToolUseVerdict{
			Outcome:     pipeline.OutcomeAllow,
			AuditFields: fields,
			Facts:       []pipeline.EvaluationFact{fact},
		}
	}
	return pipeline.ToolUseVerdict{
		Outcome:     pipeline.OutcomeDeny,
		Reason:      reason,
		AuditFields: fields,
		Facts:       []pipeline.EvaluationFact{fact},
	}
}

var _ pipeline.ToolUseEvaluator = (*BoundaryCheckEvaluator)(nil)
