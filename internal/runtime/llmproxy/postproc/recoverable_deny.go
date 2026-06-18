package postproc

import (
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// transformRecoverableDenyToPlaceholder converts a recoverable-deny
// verdict (RecoverableDenyVerdict — Outcome=Deny with RecoverableReason
// set) into a placeholder-tool_use shape and attaches a
// PendingSubstitutionSpec describing the inbound substitution.
//
// This transform is PURE: it does not write to the substitution
// registry. Centralized registration happens in
// postprocessSession.commitSubstitutions after every evaluator has
// produced its verdict, which preserves the "verdict is pure data"
// invariant and makes rollback symmetric (the layer that registers
// owns the rollback).
//
// Skipped (verdict returned unchanged) when:
//   - the verdict isn't a recoverable-deny
//   - another policy already chose a placeholder shape (e.g.
//     scope-drift sets SubstituteWithToolCall directly)
//   - the request lacks the (AgentID, ConversationID) identity tuple
//     the registry key requires — leaving SubstituteWith as the
//     terminal fallback when identity is incomplete (test fixtures /
//     partially-wired deployments)
func transformRecoverableDenyToPlaceholder(v conversation.ToolUseVerdict, tu conversation.ToolUse, cfg llmproxy.PostprocessConfig) conversation.ToolUseVerdict {
	if v.Outcome != conversation.OutcomeDeny || v.RecoverableReason == "" {
		return v
	}
	if v.SubstituteWithToolCall != nil {
		return v
	}
	if cfg.AuthorizationContext.ScopeDrifts == nil {
		return v
	}
	if cfg.AgentContext.AgentID == "" || cfg.AuditContext.ConversationID == "" {
		// Substitution keys are (AgentID, ConversationID, ToolUseID).
		// Without all three the key shape collapses across concurrent
		// conversations from the same agent (or refuses the write
		// outright on the AgentID guard), so distinct denies could
		// later restore the wrong reason into the wrong turn. Leave
		// SubstituteWith as the terminal fallback when identity is
		// incomplete — production wiring always populates both fields.
		return v
	}
	reason := v.RecoverableReason
	v.SubstituteWithToolCall = &conversation.SyntheticToolCall{
		ID:   tu.ID,
		Name: llmproxy.ScopeDriftPlaceholderToolName,
		Input: map[string]any{
			"command": llmproxy.BuildRecoverableDenyPlaceholderCommand(tu.Name, reason),
		},
	}
	v.SuppressSubstituteText = true
	v.SubstituteWith = ""
	v.RecoverableReason = ""
	v.PendingSubstitution = &conversation.PendingSubstitutionSpec{
		MenuText:          reason,
		OriginalToolName:  tu.Name,
		OriginalToolInput: append([]byte(nil), tu.Input...),
	}
	return v
}

