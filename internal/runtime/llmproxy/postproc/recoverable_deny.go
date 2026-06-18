package postproc

import (
	"context"
	"encoding/json"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// transformRecoverableDenyToPlaceholder converts a Deny+Continue
// verdict (RecoverableDenyVerdict / RecoverableContinue) into a
// placeholder-tool_use + pending-substitution shape so the model sees
// its own original tool_use answered by the reason on the next inbound
// /v1/messages — instead of the proxy issuing an upstream continuation
// call to deliver the reason as a synthetic tool_result.
//
// Allow+Continue is the inline-task auto-approve pattern and stays on
// the legacy tryContinuation flow: that path advances the conversation
// past the auto-approved call rather than asking the model to retry,
// so a placeholder swap would be semantically wrong (the model would
// see its own "create task" call answered by the result of a
// post-creation step).
//
// Returns the (possibly unchanged) verdict and, when a substitution
// was registered, the key the caller should hand to the rollback path
// so a later failClosed / rewrite-error can revert the registry write.
// Any failure to register leaves the verdict alone so the legacy
// continuation path remains the fallback.
func transformRecoverableDenyToPlaceholder(ctx context.Context, v conversation.ToolUseVerdict, tu conversation.ToolUse, cfg llmproxy.PostprocessConfig) (conversation.ToolUseVerdict, *llmproxy.PendingSubstitutionKey) {
	if v.Outcome != conversation.OutcomeDeny || v.Continue == nil {
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
		// substitution; better to leave the legacy path alone.
		return v, nil
	}
	reason, ok := extractRecoverableContinueReason(v.Continue)
	if !ok {
		return v, nil
	}
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
	v.Continue = nil
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

// extractRecoverableContinueReason pulls the reason string back out of
// a ContinueSignal built by conversation.RecoverableContinue. The
// signal carries the reason as a single JSON-marshaled string in
// SyntheticToolResults[0]; anything else is treated as "not a
// recoverable-deny we own."
func extractRecoverableContinueReason(c *conversation.ContinueSignal) (string, bool) {
	if c == nil || len(c.SyntheticToolResults) != 1 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(c.SyntheticToolResults[0], &s); err != nil {
		return "", false
	}
	return s, s != ""
}
