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
