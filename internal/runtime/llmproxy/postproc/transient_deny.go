package postproc

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// transformTransientDenyToRecoverable promotes a Deny verdict marked
// with TransientFailureClass to a RecoverableDeny on its FIRST
// occurrence per (agent, conversation, failure class) within the
// TransientBudget's TTL. The downstream transformRecoverableDenyToPlaceholder
// then converts the promoted verdict into the standard one-shot
// continuation retry shape.
//
// On subsequent occurrences (budget exhausted) the verdict passes
// through as a plain Deny so chronic failures surface to the user
// instead of looping. This realizes the "at most once per error class"
// retry policy for transient failures (judge timeout, nonce-mint
// hiccup, decision-engine RPC blip).
//
// trackConsumed, when non-nil, is called with the consumed key after a
// successful Try. The postproc session uses it to remember keys that
// must be refunded if the response is later fail-closed — otherwise
// the agent's one retry slot would burn for a recoverable response
// they never actually saw.
//
// Skipped (verdict returned unchanged) when:
//   - not a Deny verdict, or TransientFailureClass is empty
//   - RecoverableReason is already set — a prior layer already chose
//     the recoverable shape and we must not double-process
//   - TransientBudget is unconfigured — fall through to plain Deny so
//     missing wiring is loud, not silently lenient
//   - identity tuple (AgentID, ConversationID) is incomplete — the
//     budget key would collapse across distinct conversations from the
//     same agent, so degrade safely to plain Deny rather than misroute
func transformTransientDenyToRecoverable(
	ctx context.Context,
	v conversation.ToolUseVerdict,
	_ conversation.ToolUse,
	cfg llmproxy.PostprocessConfig,
	trackConsumed func(llmproxy.TransientBudgetKey),
) conversation.ToolUseVerdict {
	if v.Outcome != conversation.OutcomeDeny || v.TransientFailureClass == "" {
		return v
	}
	if v.RecoverableReason != "" {
		return v
	}
	budget := cfg.AuthorizationContext.TransientBudget
	if budget == nil {
		return v
	}
	if cfg.AgentContext.AgentID == "" || cfg.AuditContext.ConversationID == "" {
		return v
	}
	key := llmproxy.TransientBudgetKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		FailureClass:   v.TransientFailureClass,
	}
	if !budget.Try(ctx, key) {
		return v
	}
	if trackConsumed != nil {
		trackConsumed(key)
	}
	v.RecoverableReason = v.Reason
	v.SubstituteWith = v.Reason
	return v
}
