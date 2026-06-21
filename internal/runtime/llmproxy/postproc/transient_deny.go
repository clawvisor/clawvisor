// Transient-deny promotion: design constraints and why this isn't a
// deferred spec like PendingSubstitution / DeferredDriftOutcome.
//
// The other postproc transforms follow a "verdict is pure data"
// invariant: evaluators populate a spec field on the verdict; the
// transform translates it to wire shape; commitVerdictSideEffects
// realizes the spec as a registry write; rollbackVerdictSideEffects
// undoes the write. No registry mutation happens inside the transform.
//
// Transient-deny CAN'T fit that pattern because of an ordering
// constraint baked into postproc.go:
//
//   1. eval — innerEval runs, then this transform, then
//      transformRecoverableDenyToPlaceholder. The placeholder
//      transform consumes RecoverableReason and emits the placeholder
//      wire shape (SubstituteWithToolCall + PendingSubstitution).
//   2. rewrite — the rewriter walks the verdict map and emits the
//      response body using the placeholder shape.
//   3. commitVerdictSideEffects — registry writes happen here, AFTER
//      the response body has already shipped the placeholder.
//
// The budget Try() is the decision point for whether to promote the
// verdict to recoverable. If we deferred Try to step (3), step (2)
// would have already shipped the verdict as a plain Deny — too late
// to convert it. So Try MUST happen during step (1).
//
// The constraint propagates: this transform performs a session-state
// mutation (consumes a budget slot) during eval, breaking the strict
// "transform is pure" rule. We mitigate by splitting the work:
//
//   - tryPromoteTransient — pure function (ctx, verdict, cfg) →
//     (verdict, consumedKey). Encapsulates the decision; returns the
//     mutation it caused as data the caller can act on. No session
//     reference.
//   - session.applyTransientTransform — session method that wraps
//     the pure function and owns the consumed-key tracking on the
//     session, matching how session.evaluator / .finalize / .rollback
//     own their session state.
//   - rollbackVerdictSideEffects — calls TransientBudget.Release on
//     every tracked key so a fail-closed response refunds the agent's
//     retry slot. Same shape as the substitution / drift-outcome
//     rollback.
//
// If the rewrite step ever moves AFTER commitVerdictSideEffects (e.g.
// a two-pass rewrite where pass-1 discovers tool_uses and pass-2
// emits the body using post-commit verdicts), this transform could be
// converted to a true deferred spec — that restructure was considered
// and rejected as disproportionate to the single use site today
// (script-session judge timeout). Revisit if more transient sites
// land.

package postproc

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// tryPromoteTransient is the pure-function half of the transient-deny
// transform pipeline. It decides whether the Deny verdict should be
// promoted to a RecoverableDeny based on the (agent, conversation,
// failure class) budget, and returns:
//
//   - the resulting verdict (mutated only when promotion fires)
//   - the consumed key, when promotion fired — otherwise nil
//
// The session method that wraps this owns the consumed-key tracking
// for rollback; this function stays free of session state so it
// matches the pure-transform shape the other postproc transforms use
// (transformRecoverableDenyToPlaceholder etc.). The Try call must
// happen here (not in commitVerdictSideEffects) because the
// downstream transformRecoverableDenyToPlaceholder needs the promoted
// RecoverableReason BEFORE the rewriter consumes the verdict shape.
//
// Skipped (verdict returned unchanged, nil key) when:
//   - not a Deny verdict, or TransientFailureClass is empty
//   - RecoverableReason is already set — a prior layer already chose
//     the recoverable shape and we must not double-process
//   - TransientBudget is unconfigured — fall through to plain Deny so
//     missing wiring is loud, not silently lenient
//   - identity tuple (AgentID, ConversationID) is incomplete — the
//     budget key would collapse across distinct conversations from the
//     same agent, so degrade safely to plain Deny rather than misroute
//   - Try returns false (budget exhausted for this class on this
//     conversation within TTL)
func tryPromoteTransient(
	ctx context.Context,
	v conversation.ToolUseVerdict,
	cfg llmproxy.PostprocessConfig,
) (conversation.ToolUseVerdict, *llmproxy.TransientBudgetKey) {
	if v.Outcome != conversation.OutcomeDeny || v.TransientFailureClass == "" {
		return v, nil
	}
	if v.RecoverableReason != "" {
		return v, nil
	}
	budget := cfg.AuthorizationContext.TransientBudget
	if budget == nil {
		return v, nil
	}
	if cfg.AgentContext.AgentID == "" || cfg.AuditContext.ConversationID == "" {
		return v, nil
	}
	key := llmproxy.TransientBudgetKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		FailureClass:   v.TransientFailureClass,
	}
	if !budget.Try(ctx, key) {
		return v, nil
	}
	v.RecoverableReason = v.Reason
	v.SubstituteWith = v.Reason
	return v, &key
}
