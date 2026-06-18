package postproc

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// transformRecoverableDenyToPlaceholder converts a recoverable-deny
// verdict (RecoverableDenyVerdict — Outcome=Deny with RecoverableReason
// set) into a placeholder-tool_use + pending-substitution shape so the
// model sees its own original tool_use answered by the reason on the
// next inbound /v1/messages.
//
// Returns the (possibly unchanged) verdict and, when a substitution
// was registered, the key the caller should hand to the rollback path
// so a later failClosed / rewrite-error can revert the registry write.
// Any failure to register leaves the verdict alone so the SubstituteWith
// terminal fallback still surfaces the reason.
func transformRecoverableDenyToPlaceholder(ctx context.Context, v conversation.ToolUseVerdict, tu conversation.ToolUse, cfg llmproxy.PostprocessConfig) (conversation.ToolUseVerdict, *llmproxy.PendingSubstitutionKey) {
	if v.Outcome != conversation.OutcomeDeny || v.RecoverableReason == "" {
		return v, nil
	}
	// Another policy already chose a placeholder shape (scope-drift
	// sets SubstituteWithToolCall directly). Honour it.
	if v.SubstituteWithToolCall != nil {
		return v, nil
	}
	registry := cfg.AuthorizationContext.ScopeDrifts
	if registry == nil {
		return v, nil
	}
	if cfg.AgentContext.AgentID == "" {
		// Without an agent id the inbound rewriter can't key the
		// substitution; better to leave SubstituteWith as the terminal
		// fallback.
		return v, nil
	}
	reason := v.RecoverableReason
	sentinel := &conversation.SyntheticToolCall{
		ID:   tu.ID,
		Name: llmproxy.ScopeDriftPlaceholderToolName,
		Input: map[string]any{
			"command": llmproxy.BuildRecoverableDenyPlaceholderCommand(tu.Name, reason),
		},
	}
	key := llmproxy.PendingSubstitutionKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		ToolUseID:      tu.ID,
	}
	if err := registry.RegisterPendingSubstitution(ctx, key,
		llmproxy.PendingSubstitution{
			MenuText:          reason,
			OriginalToolName:  tu.Name,
			OriginalToolInput: append([]byte(nil), tu.Input...),
		},
	); err != nil {
		return v, nil
	}
	v.SubstituteWithToolCall = sentinel
	v.SuppressSubstituteText = true
	v.SubstituteWith = ""
	v.RecoverableReason = ""
	return v, &key
}

// detectScopeDriftSubstitution returns the registry key for a
// substitution that an inner evaluator (today: scope-drift mint in
// pipelineeval/scope_drift.go) wrote during this request, so the
// session rollback path can revert it on failClosed alongside the
// recoverable-deny migrations this file owns.
//
// The detection rule: a Deny verdict whose SubstituteWithToolCall is
// set, where the registry actually carries a substitution for this
// (agent, conversation, tool_use_id). Anything else (allowed siblings,
// approval-prompt substitute, recoverable-deny verdicts whose
// transformation step already returned a key) returns nil.
func detectScopeDriftSubstitution(ctx context.Context, v conversation.ToolUseVerdict, tu conversation.ToolUse, cfg llmproxy.PostprocessConfig) *llmproxy.PendingSubstitutionKey {
	if v.Outcome != conversation.OutcomeDeny || v.SubstituteWithToolCall == nil {
		return nil
	}
	registry := cfg.AuthorizationContext.ScopeDrifts
	if registry == nil || cfg.AgentContext.AgentID == "" {
		return nil
	}
	key := llmproxy.PendingSubstitutionKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		ToolUseID:      tu.ID,
	}
	if _, ok := registry.LookupPendingSubstitution(ctx, key); !ok {
		return nil
	}
	return &key
}

