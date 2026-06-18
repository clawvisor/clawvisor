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
	// ReadOnlyShellCommand reports whether the shell-specials
	// classifier determined this trigger miss is read-only.
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
	// Precomputed, when non-nil, supplies the decision a caller already
	// resolved (typically via a batched pre-pass over the response's
	// sibling tool_uses, e.g. runtimedecision.EvaluateAuthorizationBatch).
	// Evaluate uses it in place of an inline EvaluateAuthorization call —
	// the side-effect dispatch (SlideTask / HoldHandler) still runs
	// serially in the orchestrator's per-tool-use loop, so ordering is
	// preserved.
	//
	// PrecomputedErr, when non-nil, surfaces a decision-engine error
	// the pre-pass collected. It takes priority over Precomputed and
	// yields the same Deny + "decision_error" fact path the inline
	// call would have taken.
	Precomputed    *runtimedecision.AuthorizationDecision
	PrecomputedErr error
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
	// surfaces via the verdict's SubstituteWith field. Used for the
	// non-scope-drift approval-prompt path.
	SubstituteText string
	// SubstituteToolCall, when non-nil, signals the handler took the
	// scope-drift menu path. The policy returns a Deny verdict carrying
	// SubstituteWithToolCall so the response rewriter replaces the
	// blocked tool_use with the supplied placeholder (Bash no-op). The
	// inbound rewriter (running on the next /v1/messages) restores the
	// original tool_use and replaces the tool_result content with the
	// menu — see scope_drift_inbound_rewrite.go.
	SubstituteToolCall *conversation.SyntheticToolCall
	// PendingSubstitution, when non-nil, asks the postprocess layer to
	// register an inbound substitution after the verdict is finalized.
	// Populated alongside SubstituteToolCall by the scope-drift menu
	// path; the policy forwards it onto the resulting ToolUseVerdict
	// so registration happens once, centrally, after eval completes.
	PendingSubstitution *conversation.PendingSubstitutionSpec
	// Err, when non-empty, signals a hold storage failure. The policy
	// returns Deny.
	Err string
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
	var (
		dec runtimedecision.AuthorizationDecision
		err error
	)
	switch {
	case in.PrecomputedErr != nil:
		err = in.PrecomputedErr
	case in.Precomputed != nil:
		dec = *in.Precomputed
	default:
		dec, err = runtimedecision.EvaluateAuthorization(ctx, input)
	}
	if err != nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  ModelSafeInternalReason("authorization"),
			Facts:   []pipeline.EvaluationFact{pipeline.AuthorizationFact{Outcome: "decision_error", Detail: err.Error()}},
		}, nil
	}
	taskScopeFact := pipeline.TaskScopeFact{
		Reason:        dec.Reason,
		Allowed:       dec.Kind == runtimedecision.VerdictAllow,
		MatchedTaskID: taskIDFromDecision(dec),
	}
	authFact := pipeline.AuthorizationFact{Outcome: string(dec.Source)}
	if in.ShellSensitivePath && dec.Kind != runtimedecision.VerdictAllow {
		authFact.Outcome = "sensitive_path_in_read_only_shell"
	}
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
		if in.ShellPoll {
			return pipeline.ToolUseVerdict{
				Outcome: pipeline.OutcomeAllow,
				Reason:  "background-shell poll",
				Facts:   []pipeline.EvaluationFact{pipeline.AuthorizationFact{Outcome: "shell_poll_pass_through"}, authFact, taskScopeFact},
			}, nil
		}
		if dec.Source == runtimedecision.SourceTaskScopeMissing && in.ReadOnlyShellCommand {
			reason := "read-only shell command"
			outcome := "readonly_shell_pass_through"
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
				Facts:   []pipeline.EvaluationFact{authFact, taskScopeFact},
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
				Reason:  ModelSafeUnavailableReason("approval"),
				Facts:   []pipeline.EvaluationFact{authFact, taskScopeFact},
			}, nil
		}
		if held.Err != "" {
			return pipeline.ToolUseVerdict{
				Outcome: pipeline.OutcomeDeny,
				Reason:  ModelSafeUnavailableReason("approval"),
				Facts:   []pipeline.EvaluationFact{authFact, taskScopeFact},
			}, nil
		}
		// Scope-drift menu: when the hold handler chose the menu path
		// (drift mint + pending substitution), surface the placeholder
		// SyntheticToolCall as a Deny + SubstituteWithToolCall. The
		// response rewriter substitutes the blocked tool_use with the
		// placeholder; on the next inbound /v1/messages the
		// scope-drift inbound rewriter restores the original tool_use
		// and replaces the harness-supplied tool_result content with
		// the rendered menu.
		//
		// SubstituteWith is deliberately NOT set — see
		// task_scope_evaluator.go for the same rationale.
		if held.SubstituteToolCall != nil {
			return pipeline.ToolUseVerdict{
				Outcome:                pipeline.OutcomeDeny,
				Reason:                 dec.Reason,
				SubstituteWithToolCall: held.SubstituteToolCall,
				SuppressSubstituteText: true,
				PendingSubstitution:    held.PendingSubstitution,
				Facts:                  []pipeline.EvaluationFact{authFact, taskScopeFact},
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
