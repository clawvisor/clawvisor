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
// verdict needs to thread between the two steps. Modeling the chain as
// one ToolUseEvaluator preserves that information flow without
// introducing a per-tool-use state carrier in the pipeline.
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
	boundary        BoundaryResolver
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
// The legacy AllowedHostsResolver still flows through here for tests
// and call sites not yet migrated to BoundaryResolver — it's adapted
// to the typed shape via boundaryResolverFromHosts. Nil resolver →
// degraded behavior (boundary check skipped on credentialed calls).
func NewInspectorChain(insp *inspector.Inspector, resolver AllowedHostsResolver) *InspectorChain {
	return &InspectorChain{
		inspector: insp,
		boundary:  boundaryResolverFromHosts(resolver),
	}
}

// WithBoundaryResolver attaches a typed BoundaryResolver, replacing
// the legacy AllowedHostsResolver wiring. Production callers should
// prefer this so audit rows distinguish placeholder-unknown /
// ownership-mismatch / host-not-allowed denials.
func (c *InspectorChain) WithBoundaryResolver(r BoundaryResolver) *InspectorChain {
	c.boundary = r
	return c
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
		if inspector.TriggerHits(inspector.ToolUse{Input: tu.Input}) {
			return pipeline.ToolUseVerdict{
				Outcome: pipeline.OutcomeDeny,
				Reason:  "Clawvisor: credential inspection is not configured",
			}, nil
		}
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	v := c.inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})

	// Stub-placeholder guard: short `autovault_…` substrings are common
	// in tests and prose, but treating them as trigger-miss risks
	// bypassing boundary checks if the stub heuristic ever misclassifies
	// a real placeholder. Fail closed instead.
	if v.Source != inspector.SourceTriggerMiss && inspector.AllPlaceholdersAreStubs(v.Placeholders) {
		const reason = "Clawvisor: autovault placeholder is too short to validate safely"
		return conversation.RecoverableDenyVerdict(reason, newInspectorFact(v)), nil
	}

	inspectorFact := newInspectorFact(v)

	// Trigger miss: not an autovault-bearing call. If a trigger-miss
	// authorizer is configured, delegate to it (runs EvaluateAuthorization
	// + readonly-shell / sensitive-path branches). Otherwise Skip and let
	// downstream evaluators / default-Allow handle it.
	if v.Source == inspector.SourceTriggerMiss {
		if c.triggerMissAuth != nil {
			verdict := c.triggerMissAuth(ctx, tu, mut)
			verdict.Facts = append([]pipeline.EvaluationFact{inspectorFact}, verdict.Facts...)
			return verdict, nil
		}
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeSkip,
			Facts:   []pipeline.EvaluationFact{inspectorFact},
		}, nil
	}

	// Ambiguous: fail closed with per-tool HoldKey.
	if v.Ambiguous {
		return pipeline.ToolUseVerdict{
			Outcome:      pipeline.OutcomeHold,
			Reason:       v.Reason,
			HoldKey:      "ambiguous_" + tu.ID,
			HeldKindHint: pipeline.HeldKindHintApproval,
			Facts:        []pipeline.EvaluationFact{inspectorFact},
		}, nil
	}

	// Not an API call (per validator): skip so downstream/default
	// safeguards still get a chance to decide the tool_use.
	if !v.IsAPICall {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeSkip,
			Facts:   []pipeline.EvaluationFact{inspectorFact},
		}, nil
	}

	// Credentialed API call. Boundary check decides whether to fail
	// closed; Allow paths return Skip so downstream stages
	// (TaskScopeEvaluator + IntentVerifyEvaluator + CredentialRewriteEvaluator)
	// can run the credentialed authorization + rewrite flow.
	if c.boundary == nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: credentialed boundary check is not configured",
			Facts:   []pipeline.EvaluationFact{inspectorFact},
		}, nil
	}

	decision := c.boundary(ctx, v)
	placeholder := ""
	if len(v.Placeholders) > 0 {
		placeholder = v.Placeholders[0]
	}
	boundaryFact := pipeline.BoundaryFact{
		Passed:      decision.Allowed,
		DenyReason:  decision.DenyReason,
		Reason:      decision.Reason,
		Placeholder: placeholder,
		Host:        v.Host,
	}
	if !decision.Allowed {
		return conversation.RecoverableDenyVerdict(decision.Reason, inspectorFact, boundaryFact), nil
	}

	// Boundary passed — let downstream stages handle credentialed
	// authorization + rewrite.
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeSkip,
		Facts:   []pipeline.EvaluationFact{inspectorFact, boundaryFact},
	}, nil
}

// newInspectorFact extracts the typed InspectorFact from an inspector
// verdict. Used by InspectorChain + InspectorEvaluator so they emit
// the same fact shape regardless of which path runs.
func newInspectorFact(v inspector.Verdict) pipeline.InspectorFact {
	return pipeline.InspectorFact{
		Source:       string(v.Source),
		Host:         v.Host,
		Method:       v.Method,
		Path:         v.Path,
		Placeholders: append([]string(nil), v.Placeholders...),
		IsAPICall:    v.IsAPICall,
		Ambiguous:    v.Ambiguous,
		Reason:       v.Reason,
	}
}

var _ pipeline.ToolUseEvaluator = (*InspectorChain)(nil)
