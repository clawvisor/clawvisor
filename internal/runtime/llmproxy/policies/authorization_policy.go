package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
)

// AuthorizationPolicy runs runtimedecision.EvaluateAuthorization on
// trigger-miss tool_uses and translates the decision into a typed
// pipeline verdict. The Hold side-effects (PendingApprovals.Hold,
// approval-prompt rendering, evicted-task cleanup) are handled by
// PendingApprovalHoldPolicy via the AuthorizationDecision fact this
// policy emits.
//
// Decomposed from the trigger-miss authorization helper (Phase 6).
type AuthorizationPolicy struct {
	inspector *inspector.Inspector
	resolver  AuthorizationResolver
}

// AuthorizationResolver returns the per-call AuthorizationInput for a
// tool_use. Returning nil makes the policy Skip (no decision-engine
// inputs wired).
type AuthorizationResolver func(ctx context.Context, tu conversation.ToolUse, v inspector.Verdict) *AuthorizationInputs

// AuthorizationInputs is the per-call bundle the host supplies.
type AuthorizationInputs struct {
	Input runtimedecision.AuthorizationInput
	// ReadOnlyShellCommand reports whether the upstream
	// ReadOnlyShellPassthroughPolicy would have allowed this call
	// (read-only shell + agent rule). When true, the authorization's
	// SkipIntentVerification is set so the LLM intent gate doesn't
	// double-check a read-only call.
	ReadOnlyShellCommand bool
}

// NewAuthorizationPolicy constructs the policy. Nil inspector or
// resolver → Skip-always.
func NewAuthorizationPolicy(insp *inspector.Inspector, resolver AuthorizationResolver) *AuthorizationPolicy {
	return &AuthorizationPolicy{inspector: insp, resolver: resolver}
}

// Name returns the audit-friendly evaluator identifier.
func (AuthorizationPolicy) Name() string { return "authorization" }

// Evaluate runs the authorization decision and emits the result as a
// typed AuthorizationFact + Outcome.
func (p *AuthorizationPolicy) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if p.inspector == nil || p.resolver == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	v := p.inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})
	if v.Source != inspector.SourceTriggerMiss {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	in := p.resolver(ctx, tu, v)
	if in == nil {
		// Decision-engine not wired — pass-through.
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeAllow,
			Reason:  "no credential trigger",
			Facts: []pipeline.EvaluationFact{
				pipeline.ScriptSessionFact{Outcome: "pass_through"},
			},
		}, nil
	}
	input := in.Input
	input.SkipIntentVerification = in.ReadOnlyShellCommand
	dec, err := runtimedecision.EvaluateAuthorization(ctx, input)
	if err != nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: authorization failed — " + err.Error(),
			Facts: []pipeline.EvaluationFact{
				pipeline.ScriptSessionFact{Outcome: "decision_error"},
			},
		}, nil
	}
	taskScopeFact := pipeline.TaskScopeFact{
		Reason:        dec.Reason,
		Allowed:       dec.Kind == runtimedecision.VerdictAllow,
		MatchedTaskID: taskIDFromDecision(dec),
	}
	switch dec.Kind {
	case runtimedecision.VerdictAllow:
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeAllow,
			Reason:  dec.Reason,
			Facts:   []pipeline.EvaluationFact{taskScopeFact},
		}, nil
	case runtimedecision.VerdictDeny:
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: " + dec.Reason,
			Facts:   []pipeline.EvaluationFact{taskScopeFact},
		}, nil
	case runtimedecision.VerdictNeedsApproval:
		return pipeline.ToolUseVerdict{
			Outcome:      pipeline.OutcomeHold,
			Reason:       dec.Reason,
			HoldKey:      "auth_needs_approval_" + tu.ID,
			HeldKindHint: pipeline.HeldKindHintApproval,
			Facts:        []pipeline.EvaluationFact{taskScopeFact},
		}, nil
	}
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeDeny,
		Reason:  "Clawvisor: unknown decision kind",
	}, nil
}

func taskIDFromDecision(dec runtimedecision.AuthorizationDecision) string {
	if dec.Task == nil {
		return ""
	}
	return dec.Task.ID
}

var _ pipeline.ToolUseEvaluator = (*AuthorizationPolicy)(nil)
