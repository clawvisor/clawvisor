package postproc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// TestTransformRecoverableDenyToPlaceholder asserts the recoverable-
// deny migration is now a PURE transform: the verdict gets a
// placeholder SubstituteWithToolCall + a PendingSubstitution spec, but
// the registry is NOT touched. Registration is deferred to
// postprocessSession.commitSubstitutions so the verdict stays pure
// data — see TestPostprocessSessionCommitSubstitutionsRegistersSpec.
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
	got := transformRecoverableDenyToPlaceholder(original, tu, cfg)
	if got.RecoverableReason != "" {
		t.Fatalf("expected RecoverableReason cleared after migration, got %q", got.RecoverableReason)
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

	// Spec is attached to the verdict; the transform is pure. The
	// registry must NOT have been touched yet — that happens later in
	// commitSubstitutions.
	if got.PendingSubstitution == nil {
		t.Fatal("expected verdict.PendingSubstitution populated as the postproc registration spec")
	}
	if got.PendingSubstitution.MenuText != "the inspector could not parse the request body" {
		t.Fatalf("spec MenuText = %q; want the reason", got.PendingSubstitution.MenuText)
	}
	if got.PendingSubstitution.OriginalToolName != tu.Name {
		t.Fatalf("spec OriginalToolName = %q; want %q", got.PendingSubstitution.OriginalToolName, tu.Name)
	}
	if string(got.PendingSubstitution.OriginalToolInput) != string(tu.Input) {
		t.Fatalf("spec OriginalToolInput mismatch:\n got: %s\nwant: %s", string(got.PendingSubstitution.OriginalToolInput), string(tu.Input))
	}
	if _, ok := reg.LookupPendingSubstitution(context.Background(), llmproxy.PendingSubstitutionKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		ToolUseID:      tu.ID,
	}); ok {
		t.Fatal("transform must NOT write the registry; commitSubstitutions owns that side-effect")
	}
}

// TestTransformRecoverableDenyToPlaceholderLeavesNonRecoverableAlone
// guards the non-recoverable-deny gate: a verdict without
// RecoverableReason must pass through untouched (no placeholder, no
// substitution registered). Covers allow paths and terminal denies
// that explicitly opt out of the recoverable migration.
func TestTransformRecoverableDenyToPlaceholderLeavesNonRecoverableAlone(t *testing.T) {
	reg := llmproxy.NewMemoryScopeDriftRegistry(0)
	tu := conversation.ToolUse{ID: "tu-auto-1", Name: "Bash", Input: json.RawMessage(`{"command":"curl"}`)}
	verdict := conversation.ToolUseVerdict{
		Outcome: conversation.OutcomeAllow,
		Allowed: true,
	}
	cfg := llmproxy.PostprocessConfig{
		AgentContext:         llmproxy.AgentContext{AgentID: "agent-auto-1", AgentUserID: "user-auto-1"},
		AuditContext:         llmproxy.AuditContext{ConversationID: "conv-auto-1"},
		AuthorizationContext: llmproxy.AuthorizationContext{ScopeDrifts: reg},
	}
	got := transformRecoverableDenyToPlaceholder(verdict, tu, cfg)
	if got.PendingSubstitution != nil {
		t.Fatalf("non-recoverable verdict must NOT attach a spec; got %+v", *got.PendingSubstitution)
	}
	if got.SubstituteWithToolCall != nil {
		t.Fatal("non-recoverable verdict must NOT get a placeholder")
	}
	if _, ok := reg.LookupPendingSubstitution(context.Background(), llmproxy.PendingSubstitutionKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		ToolUseID:      tu.ID,
	}); ok {
		t.Fatal("non-recoverable verdict must NOT touch the registry")
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
