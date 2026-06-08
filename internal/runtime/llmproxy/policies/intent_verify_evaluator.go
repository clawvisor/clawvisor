package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// IntentVerifyEvaluator runs the LLM-backed intent check that confirms
// a tool_use's purpose matches its task scope.
//
// The verification call is supplied by the handler as a closure so the
// evaluator stays decoupled from the underlying IntentVerifier
// interface and its IntentVerifyRequest dependencies (task purpose,
// expected use, validator state). The handler closes over all of those
// at construction time.
//
// Outcomes:
//   - resolver returns ok=true → Skip with verifier_verdict fact so
//     downstream credential rewrite can still run
//   - resolver returns ok=false → Deny with the verifier's reason in
//     audit + verdict; the inspector chain's verdict authoritatively
//     refuses the tool_use
//   - resolver returns ok=false with empty reason → Deny with a generic
//     fallback; opt-out is represented by ok=true, reason="".
type IntentVerifyEvaluator struct {
	resolver IntentVerifyResolver
}

// IntentVerifyResolver returns the verifier's decision for a tool_use.
// The handler implements this against the IntentVerifier instance,
// closing over the IntentVerifyRequest's identity and scope inputs.
//
// Returns (ok=true, reason="") on Allow or opt-out, and
// (ok=false, reason=<verdict>) on Deny. Empty deny reasons are denied
// with a generic fallback instead of being treated as opt-out.
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
		// Verifier passed silently. Do not claim the tool_use; the
		// downstream credential rewrite still needs to run.
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeSkip,
			Facts:   []pipeline.EvaluationFact{pipeline.IntentVerifyFact{Allowed: true, Outcome: "intent_verification_passed"}},
		}, nil
	}
	if reason == "" && !ok {
		reason = "Clawvisor: intent verification denied this tool use"
	}

	fact := pipeline.IntentVerifyFact{Allowed: ok, Explanation: reason}

	if ok {
		fact.Outcome = "intent_verification_passed"
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeSkip,
			Facts:   []pipeline.EvaluationFact{fact},
		}, nil
	}
	fact.Outcome = "intent_verification_failed"
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeDeny,
		Reason:  reason,
		Facts:   []pipeline.EvaluationFact{fact},
	}, nil
}

var _ pipeline.ToolUseEvaluator = (*IntentVerifyEvaluator)(nil)
