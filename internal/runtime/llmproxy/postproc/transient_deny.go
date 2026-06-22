package postproc

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// transientConsume identifies a successful Try call so a later
// rollback can refund the EXACT slot it took (not a slot that has
// since rotated to a different consumer). Token-checked Release is
// what keeps a delayed rollback from clobbering an unrelated request's
// budget.
type transientConsume struct {
	Key   llmproxy.TransientBudgetKey
	Token llmproxy.TransientReleaseToken
}

// tryPromoteTransient is the pure-function decision for whether a
// Deny verdict tagged with TransientFailureClass should be promoted
// to a RecoverableDeny on this attempt. Returns the (possibly
// promoted) verdict and the consume record — nil when no promotion
// fired.
//
// Called from session.promoteTransients during commitVerdictSideEffects.
// The wrapping session method owns the consume tracking for rollback
// so this function stays free of session state, matching the pure
// shape transformRecoverableDenyToPlaceholder uses.
//
// Skipped (verdict returned unchanged, nil consume) when:
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
) (conversation.ToolUseVerdict, *transientConsume) {
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
	token, ok := budget.Try(ctx, key)
	if !ok {
		return v, nil
	}
	v.RecoverableReason = v.Reason
	v.SubstituteWith = v.Reason
	return v, &transientConsume{Key: key, Token: token}
}
