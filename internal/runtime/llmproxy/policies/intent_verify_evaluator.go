package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// IntentVerifyEvaluator runs the LLM-backed intent check that confirms
// a tool_use's purpose matches its task scope. Phase 4's fourth and
// final inspector-chain evaluator.
//
// The verification call is supplied by the handler as a closure so the
// evaluator stays decoupled from the underlying IntentVerifier
// interface and its IntentVerifyRequest dependencies (task purpose,
// expected use, validator state). The handler closes over all of those
// at construction time.
//
// Outcomes:
//   - resolver returns ok=true → Allow with verifier_verdict in audit
//   - resolver returns ok=false → Deny with the verifier's reason in
//     audit + verdict; the inspector chain's verdict authoritatively
//     refuses the tool_use
//   - resolver returns empty reason → Skip (verifier chose not to act —
//     e.g., no task scope to verify against)
type IntentVerifyEvaluator struct {
	resolver IntentVerifyResolver
}

// IntentVerifyResolver returns the verifier's decision for a tool_use.
// The handler implements this against the IntentVerifier instance,
// closing over the IntentVerifyRequest's identity and scope inputs.
//
// Returns (ok=true, reason="") on Allow, (ok=false, reason=<verdict>)
// on Deny. Empty reason on both fields signals "verifier chose not to
// act" — the evaluator emits Skip.
type IntentVerifyResolver func(ctx context.Context, tu conversation.ToolUse) (ok bool, reason string)

// NewIntentVerifyEvaluator constructs the evaluator. nil resolver → Skip.
func NewIntentVerifyEvaluator(resolver IntentVerifyResolver) *IntentVerifyEvaluator {
	return &IntentVerifyEvaluator{resolver: resolver}
}

// Name returns the audit-friendly identifier.
func (IntentVerifyEvaluator) Name() string { return "intent_verify" }

// Evaluate dispatches to the resolver and translates the decision
// into a pipeline verdict.
func (e *IntentVerifyEvaluator) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if e.resolver == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	ok, reason := e.resolver(ctx, tu)
	if reason == "" && ok {
		// Allowed with no reason — verifier passed silently. Allow.
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeAllow,
			AuditFields: map[string]any{
				"intent_verifier_passed": true,
			},
			Facts: []pipeline.EvaluationFact{pipeline.IntentVerifyFact{Allowed: true}},
		}, nil
	}
	if reason == "" && !ok {
		// Verifier chose not to act (no scope to verify against, etc.).
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	fields := map[string]any{
		"intent_verifier_passed": ok,
		"intent_verifier_reason": reason,
	}
	fact := pipeline.IntentVerifyFact{Allowed: ok, Explanation: reason}

	if ok {
		return pipeline.ToolUseVerdict{
			Outcome:     pipeline.OutcomeAllow,
			AuditFields: fields,
			Facts:       []pipeline.EvaluationFact{fact},
		}, nil
	}
	return pipeline.ToolUseVerdict{
		Outcome:     pipeline.OutcomeDeny,
		Reason:      reason,
		AuditFields: fields,
		Facts:       []pipeline.EvaluationFact{fact},
	}, nil
}

var _ pipeline.ToolUseEvaluator = (*IntentVerifyEvaluator)(nil)
