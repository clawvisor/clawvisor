package postproc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// TestTransformRecoverableDenyToPlaceholder asserts the Continue+Deny
// verdict shape produced by conversation.RecoverableDenyVerdict is
// migrated to the placeholder + pending-substitution pattern: the
// model's original tool_use is captured for restoration, the menu
// (reason) text is stashed for the next inbound /v1/messages, and the
// verdict's Continue signal is cleared so tryContinuation no longer
// fires for these cases.
func TestTransformRecoverableDenyToPlaceholder(t *testing.T) {
	reg := llmproxy.NewMemoryScopeDriftRegistry(0)
	tu := conversation.ToolUse{
		ID:    "tu-recover-1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl -X POST https://invalid"}`),
	}
	original := conversation.RecoverableDenyVerdict("the inspector could not parse the request body")
	cfg := llmproxy.PostprocessConfig{
		AgentContext: llmproxy.AgentContext{AgentID: "agent-recover-1", AgentUserID: "user-recover-1"},
		AuditContext: llmproxy.AuditContext{ConversationID: "conv-recover-1"},
		AuthorizationContext: llmproxy.AuthorizationContext{
			ScopeDrifts: reg,
		},
	}
	got := transformRecoverableDenyToPlaceholder(context.Background(), original, tu, cfg)
	if got.Continue != nil {
		t.Fatalf("expected Continue signal cleared after migration, got %+v", got.Continue)
	}
	if got.SubstituteWith != "" {
		t.Fatalf("expected SubstituteWith cleared (placeholder owns the wire shape), got %q", got.SubstituteWith)
	}
	if !got.SuppressSubstituteText {
		t.Fatal("expected SuppressSubstituteText=true on migrated verdict")
	}
	if got.SubstituteWithToolCall == nil {
		t.Fatal("expected SubstituteWithToolCall populated with placeholder")
	}
	if got.SubstituteWithToolCall.ID != tu.ID {
		t.Fatalf("placeholder must preserve tool_use_id: got %q want %q", got.SubstituteWithToolCall.ID, tu.ID)
	}
	if got.SubstituteWithToolCall.Name != llmproxy.ScopeDriftPlaceholderToolName {
		t.Fatalf("placeholder should use canonical Bash name; got %q", got.SubstituteWithToolCall.Name)
	}
	cmd, _ := got.SubstituteWithToolCall.Input["command"].(string)
	if cmd == "" {
		t.Fatalf("placeholder input missing command: %+v", got.SubstituteWithToolCall.Input)
	}

	// Pending substitution is keyed on (agent, conversation, tool_use_id);
	// carries the reason as MenuText and the original tool shape for
	// later restoration.
	subst, ok := reg.LookupPendingSubstitution(context.Background(), llmproxy.PendingSubstitutionKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		ToolUseID:      tu.ID,
	})
	if !ok {
		t.Fatal("expected pending substitution registered for recoverable-deny tool_use")
	}
	if subst.MenuText != "the inspector could not parse the request body" {
		t.Fatalf("substitution menu text = %q; want the reason", subst.MenuText)
	}
	if subst.OriginalToolName != tu.Name {
		t.Fatalf("substitution original tool name = %q; want %q", subst.OriginalToolName, tu.Name)
	}
	if string(subst.OriginalToolInput) != string(tu.Input) {
		t.Fatalf("substitution original tool input mismatch:\n got: %s\nwant: %s", string(subst.OriginalToolInput), string(tu.Input))
	}
}

// TestTransformRecoverableDenyToPlaceholderLeavesAutoApproveAlone
// guards the inline-task auto-approve flow: its verdict has
// Outcome=Allow + Continue, NOT Outcome=Deny + Continue. That path
// must stay on the upstream-continuation flow because it advances the
// conversation past the auto-approved call rather than expecting the
// model to retry.
func TestTransformRecoverableDenyToPlaceholderLeavesAutoApproveAlone(t *testing.T) {
	reg := llmproxy.NewMemoryScopeDriftRegistry(0)
	tu := conversation.ToolUse{ID: "tu-auto-1", Name: "Bash", Input: json.RawMessage(`{"command":"curl"}`)}
	payload, _ := json.Marshal("task created, proceed")
	verdict := conversation.ToolUseVerdict{
		Outcome: conversation.OutcomeAllow,
		Allowed: true,
		Continue: &conversation.ContinueSignal{
			SyntheticToolResults: []json.RawMessage{payload},
		},
	}
	cfg := llmproxy.PostprocessConfig{
		AgentContext: llmproxy.AgentContext{AgentID: "agent-auto-1", AgentUserID: "user-auto-1"},
		AuditContext: llmproxy.AuditContext{ConversationID: "conv-auto-1"},
		AuthorizationContext: llmproxy.AuthorizationContext{ScopeDrifts: reg},
	}
	got := transformRecoverableDenyToPlaceholder(context.Background(), verdict, tu, cfg)
	if got.Continue == nil {
		t.Fatal("auto-approve Continue signal must be preserved")
	}
	if got.SubstituteWithToolCall != nil {
		t.Fatal("auto-approve verdict must NOT get a placeholder")
	}
	if _, ok := reg.LookupPendingSubstitution(context.Background(), llmproxy.PendingSubstitutionKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		ToolUseID:      tu.ID,
	}); ok {
		t.Fatal("auto-approve must NOT register a pending substitution")
	}
}
