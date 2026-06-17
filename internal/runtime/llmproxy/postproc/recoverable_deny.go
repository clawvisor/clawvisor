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
// Returns the (possibly unchanged) verdict. Any failure to register
// the substitution leaves the verdict alone so the legacy continuation
// path remains the fallback.
func transformRecoverableDenyToPlaceholder(ctx context.Context, v conversation.ToolUseVerdict, tu conversation.ToolUse, cfg llmproxy.PostprocessConfig) conversation.ToolUseVerdict {
	if v.Outcome != conversation.OutcomeDeny || v.Continue == nil {
		return v
	}
	// Another policy already chose a placeholder shape (scope-drift
	// sets SubstituteWithToolCall directly). Honour it.
	if v.SubstituteWithToolCall != nil {
		return v
	}
	registry := cfg.AuthorizationContext.ScopeDrifts
	if registry == nil {
		return v
	}
	if cfg.AgentContext.AgentID == "" {
		// Without an agent id the inbound rewriter can't key the
		// substitution; better to leave the legacy path alone.
		return v
	}
	reason, ok := extractRecoverableContinueReason(v.Continue)
	if !ok {
		return v
	}
	sentinel := &conversation.SyntheticToolCall{
		ID:   tu.ID,
		Name: llmproxy.ScopeDriftPlaceholderToolName,
		Input: map[string]any{
			"command": llmproxy.BuildRecoverableDenyPlaceholderCommand(tu.Name, reason),
		},
	}
	if err := registry.RegisterPendingSubstitution(ctx,
		llmproxy.PendingSubstitutionKey{
			AgentID:        cfg.AgentContext.AgentID,
			ConversationID: cfg.AuditContext.ConversationID,
			ToolUseID:      tu.ID,
		},
		llmproxy.PendingSubstitution{
			MenuText:          reason,
			OriginalToolName:  tu.Name,
			OriginalToolInput: append([]byte(nil), tu.Input...),
		},
	); err != nil {
		return v
	}
	v.SubstituteWithToolCall = sentinel
	v.SuppressSubstituteText = true
	v.SubstituteWith = ""
	v.Continue = nil
	return v
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
