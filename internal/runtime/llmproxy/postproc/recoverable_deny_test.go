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
	got, registered := transformRecoverableDenyToPlaceholder(context.Background(), original, tu, cfg)
	if registered == nil {
		t.Fatal("expected registered key from transform so rollback can revert on failure")
	}
	if registered.AgentID != cfg.AgentContext.AgentID || registered.ToolUseID != tu.ID {
		t.Fatalf("registered key mismatch: %+v", *registered)
	}
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
	got, registered := transformRecoverableDenyToPlaceholder(context.Background(), verdict, tu, cfg)
	if registered != nil {
		t.Fatalf("auto-approve must NOT register a substitution; got key %+v", *registered)
	}
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

// TestDetectScopeDriftSubstitutionTracksScopeDriftMint locks the
// centralized rollback: when an inner evaluator (e.g., scope-drift
// mint in pipelineeval/scope_drift.go) registers a substitution
// during innerEval and returns a verdict with SubstituteWithToolCall,
// the postproc eval wrapper's detector picks it up so session
// rollback covers both recoverable-deny and scope-drift writes.
func TestDetectScopeDriftSubstitutionTracksScopeDriftMint(t *testing.T) {
	reg := llmproxy.NewMemoryScopeDriftRegistry(0)
	tu := conversation.ToolUse{ID: "tu-scope-drift", Name: "Bash", Input: json.RawMessage(`{"command":":"}`)}
	cfg := llmproxy.PostprocessConfig{
		AgentContext:         llmproxy.AgentContext{AgentID: "agent-x", AgentUserID: "user-x"},
		AuditContext:         llmproxy.AuditContext{ConversationID: "conv-x"},
		AuthorizationContext: llmproxy.AuthorizationContext{ScopeDrifts: reg},
	}

	// Simulate the scope-drift mint write happening inside innerEval.
	mintKey := llmproxy.PendingSubstitutionKey{
		AgentID: cfg.AgentContext.AgentID, ConversationID: cfg.AuditContext.ConversationID, ToolUseID: tu.ID,
	}
	if err := reg.RegisterPendingSubstitution(context.Background(), mintKey, llmproxy.PendingSubstitution{
		MenuText: "scope-drift menu", OriginalToolName: "Write", OriginalToolInput: []byte(`{}`),
	}); err != nil {
		t.Fatalf("RegisterPendingSubstitution: %v", err)
	}
	verdict := conversation.ToolUseVerdict{
		Outcome:                conversation.OutcomeDeny,
		SubstituteWithToolCall: &conversation.SyntheticToolCall{ID: tu.ID, Name: "Bash", Input: map[string]any{"command": ":"}},
		SuppressSubstituteText: true,
	}
	key := detectScopeDriftSubstitution(context.Background(), verdict, tu, cfg)
	if key == nil {
		t.Fatal("expected detectScopeDriftSubstitution to return the mint's key")
	}
	if *key != mintKey {
		t.Fatalf("detected key mismatch: got %+v want %+v", *key, mintKey)
	}
}

// TestDeletePendingSubstitutionRollback locks the rollback path the
// transformRecoverableDenyToPlaceholder callers rely on: a registry
// write that happens during a request whose response is later
// failClosed'd must be revertible so it doesn't survive as an orphan.
func TestDeletePendingSubstitutionRollback(t *testing.T) {
	reg := llmproxy.NewMemoryScopeDriftRegistry(0)
	key := llmproxy.PendingSubstitutionKey{
		AgentID:        "agent-rollback",
		ConversationID: "conv-rollback",
		ToolUseID:      "tu-rollback",
	}
	if err := reg.RegisterPendingSubstitution(context.Background(), key, llmproxy.PendingSubstitution{
		MenuText:          "reason",
		OriginalToolName:  "Bash",
		OriginalToolInput: []byte(`{"command":":"}`),
	}); err != nil {
		t.Fatalf("RegisterPendingSubstitution: %v", err)
	}
	if _, ok := reg.LookupPendingSubstitution(context.Background(), key); !ok {
		t.Fatal("substitution should be registered before rollback")
	}
	reg.DeletePendingSubstitution(context.Background(), key)
	if _, ok := reg.LookupPendingSubstitution(context.Background(), key); ok {
		t.Fatal("substitution should be gone after delete")
	}
}
