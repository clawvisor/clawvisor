package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// AuthorizationPolicy runs runtimedecision.EvaluateAuthorization on
// trigger-miss tool_uses and translates the decision into a typed
// pipeline verdict. The Hold side-effects (PendingApprovals.Hold +
// approval-prompt rendering + evicted-task cleanup) run inline so the
// final verdict carries the approval ID in its substitute text — the
// chain's first-non-Skip-wins shape doesn't support post-claim
// handoffs to a separate PendingApprovalHoldPolicy.
type AuthorizationPolicy struct {
	inspector *inspector.Inspector
	resolver  AuthorizationResolver
}

// AuthorizationResolver returns the per-call AuthorizationInputs for
// a tool_use. nil → Skip (no decision-engine inputs wired).
type AuthorizationResolver func(ctx context.Context, tu conversation.ToolUse, v inspector.Verdict) *AuthorizationInputs

// AuthorizationInputs is the per-call bundle the host supplies.
type AuthorizationInputs struct {
	Input runtimedecision.AuthorizationInput
	// HasPolicyConfig reports whether the host wired any
	// decision-engine inputs (CandidateTasks, ToolRules, EgressRules).
	// When false AND ShellSensitivePath is also false, the policy
	// short-circuits with the "no credential trigger" pass-through
	// Allow.
	HasPolicyConfig bool
	// ShellSensitivePath signals that an upstream sensitive-path
	// detection fired. Forces EvaluateAuthorization to run even when
	// no policy config is wired so sensitive-path + no-policy still
	// routes through the approval flow.
	ShellSensitivePath bool
	// ReadOnlyShellCommand reports whether the upstream
	// ReadOnlyShellPassthroughPolicy would have allowed this call.
	// Set true → SkipIntentVerification on the authorization input.
	ReadOnlyShellCommand bool
	// ShellPoll reports whether this is a no-op background shell poll
	// (`write_stdin` with empty chars). It is allowed only after
	// EvaluateAuthorization confirms there is no explicit deny.
	ShellPoll bool
	// HoldHandler, when non-nil, is invoked on VerdictNeedsApproval to
	// commit the hold + render the approval prompt. The policy
	// supplies the inspector verdict + decision; the handler returns
	// the rendered prompt + cleanup callback.
	HoldHandler AuthorizationHoldHandler
	// SlideTask, when non-nil, is invoked on VerdictAllow with a
	// matched task so the task's sliding lifetime bumps. The handler
	// closes over the store and time.Now.
	SlideTask func(ctx context.Context, task *store.Task)
}

// AuthorizationHoldHandler is the typed interface PostprocessConfig
// supplies to AuthorizationPolicy for the approval flow.
type AuthorizationHoldHandler interface {
	Hold(ctx context.Context, req AuthorizationHoldRequest) (AuthorizationHoldResult, error)
}

// AuthorizationHoldRequest is the typed input to HoldHandler.Hold.
type AuthorizationHoldRequest struct {
	ToolUse          conversation.ToolUse
	InspectorVerdict inspector.Verdict
	Decision         runtimedecision.AuthorizationDecision
	Input            runtimedecision.AuthorizationInput
}

// AuthorizationHoldResult is the typed output of HoldHandler.Hold.
type AuthorizationHoldResult struct {
	// ApprovalID is the hold ID; embedded in the approval prompt's
	// footer so subsequent y/n replies disambiguate.
	ApprovalID string
	// SubstituteText is the rendered approval prompt the policy
	// surfaces via the verdict's SubstituteWith field.
	SubstituteText string
	// Err, when non-empty, signals a hold storage failure. The policy
	// returns Deny.
	Err string
	// EvictedSummary is non-empty when the hold displaced an older
	// pending approval; the policy emits a secondary audit row via
	// fact.
	EvictedSummary string
}

// NewAuthorizationPolicy constructs the policy. Nil inspector or
// resolver → Skip-always.
func NewAuthorizationPolicy(insp *inspector.Inspector, resolver AuthorizationResolver) *AuthorizationPolicy {
	return &AuthorizationPolicy{inspector: insp, resolver: resolver}
}

// Name returns the audit-friendly evaluator identifier.
func (AuthorizationPolicy) Name() string { return "authorization" }

// Evaluate runs the authorization decision and emits the result as a
// typed verdict + facts.
func (p *AuthorizationPolicy) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if p.inspector == nil || p.resolver == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	v := p.inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})
	// Stub-placeholder downgrade matches the legacy helper.
	if v.Source != inspector.SourceTriggerMiss && inspector.AllPlaceholdersAreStubs(v.Placeholders) {
		v = inspector.Verdict{
			IsAPICall: false,
			Source:    inspector.SourceTriggerMiss,
			Reason:    "placeholders are stub-length (no real vault reference)",
		}
	}
	if v.Source != inspector.SourceTriggerMiss {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	in := p.resolver(ctx, tu, v)
	if in == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if !in.HasPolicyConfig && !in.ShellSensitivePath {
		// No decision-engine inputs wired and no sensitive-path
		// override — pass-through. Matches the legacy "no credential
		// trigger" branch.
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeAllow,
			Reason:  "no credential trigger",
			Facts:   []pipeline.EvaluationFact{pipeline.AuthorizationFact{Outcome: "pass_through"}},
		}, nil
	}
	input := in.Input
	input.SkipIntentVerification = in.ReadOnlyShellCommand
	dec, err := runtimedecision.EvaluateAuthorization(ctx, input)
	if err != nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: authorization failed — " + err.Error(),
			Facts:   []pipeline.EvaluationFact{pipeline.AuthorizationFact{Outcome: "decision_error"}},
		}, nil
	}
	taskScopeFact := pipeline.TaskScopeFact{
		Reason:        dec.Reason,
		Allowed:       dec.Kind == runtimedecision.VerdictAllow,
		MatchedTaskID: taskIDFromDecision(dec),
	}
	authFact := pipeline.AuthorizationFact{Outcome: string(dec.Source)}
	switch dec.Kind {
	case runtimedecision.VerdictAllow:
		if dec.Task != nil && in.SlideTask != nil {
			in.SlideTask(ctx, dec.Task)
		}
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeAllow,
			Reason:  dec.Reason,
			Facts:   []pipeline.EvaluationFact{authFact, taskScopeFact},
		}, nil
	case runtimedecision.VerdictDeny:
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: " + dec.Reason,
			Facts:   []pipeline.EvaluationFact{authFact, taskScopeFact},
		}, nil
	case runtimedecision.VerdictNeedsApproval:
		if dec.Source == runtimedecision.SourceTaskScopeMissing && (in.ReadOnlyShellCommand || in.ShellPoll) {
			reason := "read-only shell command"
			outcome := "readonly_shell_pass_through"
			if in.ShellPoll {
				reason = "background-shell poll"
				outcome = "shell_poll_pass_through"
			}
			return pipeline.ToolUseVerdict{
				Outcome: pipeline.OutcomeAllow,
				Reason:  reason,
				Facts:   []pipeline.EvaluationFact{pipeline.AuthorizationFact{Outcome: outcome}, authFact, taskScopeFact},
			}, nil
		}
		// Hold side-effects inline so the verdict carries the rendered
		// approval prompt (with the approval ID in its footer).
		if in.HoldHandler == nil {
			// Fail-closed-without-cache: refuse so the chain's
			// downstream rewriter renders an "approval unavailable"
			// notice.
			return pipeline.ToolUseVerdict{
				Outcome: pipeline.OutcomeDeny,
				Reason:  "Clawvisor: approval unavailable",
				Facts:   []pipeline.EvaluationFact{taskScopeFact},
			}, nil
		}
		held, holdErr := in.HoldHandler.Hold(ctx, AuthorizationHoldRequest{
			ToolUse:          tu,
			InspectorVerdict: v,
			Decision:         dec,
			Input:            input,
		})
		if holdErr != nil {
			return pipeline.ToolUseVerdict{
				Outcome: pipeline.OutcomeDeny,
				Reason:  "Clawvisor: approval unavailable — " + holdErr.Error(),
				Facts:   []pipeline.EvaluationFact{taskScopeFact},
			}, nil
		}
		if held.Err != "" {
			return pipeline.ToolUseVerdict{
				Outcome: pipeline.OutcomeDeny,
				Reason:  "Clawvisor: approval unavailable — " + held.Err,
				Facts:   []pipeline.EvaluationFact{taskScopeFact},
			}, nil
		}
		return pipeline.ToolUseVerdict{
			Outcome:        pipeline.OutcomeHold,
			Reason:         "Clawvisor: approval required — " + dec.Reason,
			SubstituteWith: held.SubstituteText,
			HoldKey:        "auth_needs_approval_" + tu.ID,
			HeldKindHint:   pipeline.HeldKindHintApproval,
			Facts:          []pipeline.EvaluationFact{authFact, taskScopeFact},
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
