package postproc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// Shared fixture: a Deny verdict tagged with a failure class + base
// PostprocessConfig wired with an identity tuple and a fresh transient
// budget. Returns the verdict, an unused tool_use (kept for callers
// that need it for end-to-end tests), and the cfg.
func transientDenyTestSetup(failureClass string) (conversation.ToolUseVerdict, conversation.ToolUse, llmproxy.PostprocessConfig) {
	tu := conversation.ToolUse{
		ID:    "tu-transient-1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl https://example.com"}`),
	}
	v := conversation.TransientDenyVerdict(failureClass, "Clawvisor: judge timed out. Please retry.")
	cfg := llmproxy.PostprocessConfig{
		AgentContext: llmproxy.AgentContext{AgentID: "agent-transient", AgentUserID: "user-transient"},
		AuditContext: llmproxy.AuditContext{ConversationID: "conv-transient"},
		AuthorizationContext: llmproxy.AuthorizationContext{
			TransientBudget: llmproxy.NewMemoryTransientBudget(0),
		},
	}
	return v, tu, cfg
}

// Allow / Skip verdicts must pass through untouched and yield no key.
func TestTryPromoteTransient_LeavesNonTransientAlone(t *testing.T) {
	_, _, cfg := transientDenyTestSetup("any")
	verdict := conversation.ToolUseVerdict{Outcome: conversation.OutcomeAllow, Allowed: true}
	got, key := tryPromoteTransient(context.Background(), verdict, cfg)
	if got.RecoverableReason != "" {
		t.Fatalf("non-transient verdict must not gain RecoverableReason; got %q", got.RecoverableReason)
	}
	if got.TransientFailureClass != "" {
		t.Fatalf("non-transient verdict must not gain TransientFailureClass; got %q", got.TransientFailureClass)
	}
	if key != nil {
		t.Fatalf("non-transient verdict must not yield a consumed key; got %+v", key)
	}
}

// Plain Deny without TransientFailureClass must pass through.
func TestTryPromoteTransient_LeavesPlainDenyAlone(t *testing.T) {
	_, _, cfg := transientDenyTestSetup("any")
	verdict := conversation.ToolUseVerdict{Outcome: conversation.OutcomeDeny, Reason: "plain"}
	got, key := tryPromoteTransient(context.Background(), verdict, cfg)
	if got.RecoverableReason != "" {
		t.Fatalf("plain Deny must not be promoted; got RecoverableReason=%q", got.RecoverableReason)
	}
	if key != nil {
		t.Fatalf("plain Deny must not yield a consumed key; got %+v", key)
	}
}

// If RecoverableReason is already set, the transient transform must not
// touch it — a prior layer chose the recoverable shape and re-running
// would be a double-process. Likewise no key is consumed.
func TestTryPromoteTransient_LeavesRecoverableAlone(t *testing.T) {
	_, _, cfg := transientDenyTestSetup("class")
	v := conversation.TransientDenyVerdict("class", "transient reason")
	v.RecoverableReason = "already recoverable"
	got, key := tryPromoteTransient(context.Background(), v, cfg)
	if got.RecoverableReason != "already recoverable" {
		t.Fatalf("transform must not overwrite an existing RecoverableReason; got %q", got.RecoverableReason)
	}
	if key != nil {
		t.Fatalf("must not consume budget for already-recoverable verdict; got %+v", key)
	}
}

// Missing identity tuple → safe degrade to plain Deny.
func TestTryPromoteTransient_RequiresIdentity(t *testing.T) {
	v, _, cfg := transientDenyTestSetup("class")
	cfg.AgentContext.AgentID = ""
	got, key := tryPromoteTransient(context.Background(), v, cfg)
	if got.RecoverableReason != "" || key != nil {
		t.Fatalf("missing AgentID should NOT promote; got reason=%q key=%+v", got.RecoverableReason, key)
	}

	v2, _, cfg2 := transientDenyTestSetup("class")
	cfg2.AuditContext.ConversationID = ""
	got2, key2 := tryPromoteTransient(context.Background(), v2, cfg2)
	if got2.RecoverableReason != "" || key2 != nil {
		t.Fatalf("missing ConversationID should NOT promote; got reason=%q key=%+v", got2.RecoverableReason, key2)
	}
}

// Nil budget → safe degrade to plain Deny.
func TestTryPromoteTransient_RequiresBudget(t *testing.T) {
	v, _, cfg := transientDenyTestSetup("class")
	cfg.AuthorizationContext.TransientBudget = nil
	got, key := tryPromoteTransient(context.Background(), v, cfg)
	if got.RecoverableReason != "" || key != nil {
		t.Fatalf("nil budget should NOT promote; got reason=%q key=%+v", got.RecoverableReason, key)
	}
}

// First call: verdict is promoted (RecoverableReason + SubstituteWith
// populated, TransientFailureClass preserved) AND the consumed key is
// returned so the wrapping session method can track it for rollback.
func TestTryPromoteTransient_FirstCallPromotesAndReturnsKey(t *testing.T) {
	v, _, cfg := transientDenyTestSetup("class-x")
	got, key := tryPromoteTransient(context.Background(), v, cfg)
	if got.RecoverableReason != v.Reason {
		t.Fatalf("first call should set RecoverableReason; got %q, want %q", got.RecoverableReason, v.Reason)
	}
	if got.SubstituteWith != v.Reason {
		t.Fatalf("first call should set SubstituteWith; got %q", got.SubstituteWith)
	}
	if got.TransientFailureClass != "class-x" {
		t.Fatalf("TransientFailureClass should be preserved; got %q", got.TransientFailureClass)
	}
	want := llmproxy.TransientBudgetKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		FailureClass:   "class-x",
	}
	if key == nil || *key != want {
		t.Fatalf("expected consumed key %+v; got %+v", want, key)
	}
}

// Second call same (agent, conv, class): budget exhausted, no
// promotion, no key returned. Pins the contract the session rollback
// relies on (returned-key list = actual consumes).
func TestTryPromoteTransient_SecondCallReturnsNoKey(t *testing.T) {
	v, _, cfg := transientDenyTestSetup("class-x")
	if _, key := tryPromoteTransient(context.Background(), v, cfg); key == nil {
		t.Fatal("precondition: first call should consume budget")
	}
	v2 := conversation.TransientDenyVerdict("class-x", "retry reason")
	got, key := tryPromoteTransient(context.Background(), v2, cfg)
	if got.RecoverableReason != "" {
		t.Fatalf("second call should not promote (budget exhausted); got %q", got.RecoverableReason)
	}
	if key != nil {
		t.Fatalf("second call should NOT yield a key when Try returned false; got %+v", key)
	}
	if got.Outcome != conversation.OutcomeDeny {
		t.Fatalf("second call should remain Deny; got %v", got.Outcome)
	}
}

// Different failure class on the same (agent, conv): independent budget.
func TestTryPromoteTransient_DistinctClassesIndependent(t *testing.T) {
	_, _, cfg := transientDenyTestSetup("class-a")
	vA := conversation.TransientDenyVerdict("class-a", "reason A")
	if _, key := tryPromoteTransient(context.Background(), vA, cfg); key == nil {
		t.Fatal("class-a first call should promote")
	}
	vB := conversation.TransientDenyVerdict("class-b", "reason B")
	if _, key := tryPromoteTransient(context.Background(), vB, cfg); key == nil {
		t.Fatal("class-b first call should promote independently of class-a")
	}
}

// Different conversation on the same class: independent budget.
func TestTryPromoteTransient_DistinctConversationsIndependent(t *testing.T) {
	v1, _, cfg1 := transientDenyTestSetup("class-x")
	if _, key := tryPromoteTransient(context.Background(), v1, cfg1); key == nil {
		t.Fatal("conv-1 first call should promote")
	}
	v2, _, cfg2 := transientDenyTestSetup("class-x")
	cfg2.AuditContext.ConversationID = "conv-different"
	cfg2.AuthorizationContext.TransientBudget = cfg1.AuthorizationContext.TransientBudget
	if _, key := tryPromoteTransient(context.Background(), v2, cfg2); key == nil {
		t.Fatal("conv-2 first call should promote even though conv-1 already consumed its budget")
	}
}

// applyTransientTransform wraps the pure function with session-owned
// tracking. The wrapper IS the call site the eval closures use, so
// it's worth pinning that:
//   - it returns the same promoted verdict the pure function does
//   - it accumulates the consumed key on the session for rollback
//   - on a non-promote, it accumulates nothing (no spurious entries)
func TestSession_ApplyTransientTransform_TracksConsumeForRollback(t *testing.T) {
	v, _, cfg := transientDenyTestSetup("class-x")
	session := newPostprocessSession(cfg)
	got := session.applyTransientTransform(context.Background(), v, cfg)
	if got.RecoverableReason == "" {
		t.Fatal("first call should promote (precondition)")
	}
	if len(session.transientConsumed) != 1 {
		t.Fatalf("expected 1 tracked consume; got %d (%+v)", len(session.transientConsumed), session.transientConsumed)
	}
	want := llmproxy.TransientBudgetKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		FailureClass:   "class-x",
	}
	if session.transientConsumed[0] != want {
		t.Fatalf("tracked key = %+v; want %+v", session.transientConsumed[0], want)
	}
}

func TestSession_ApplyTransientTransform_DoesNotTrackNonPromotes(t *testing.T) {
	v, _, cfg := transientDenyTestSetup("class-x")
	session := newPostprocessSession(cfg)
	if got := session.applyTransientTransform(context.Background(), v, cfg); got.RecoverableReason == "" {
		t.Fatal("precondition: first call should promote")
	}
	// Second call same class — budget exhausted, must not track.
	v2 := conversation.TransientDenyVerdict("class-x", "retry reason")
	session.applyTransientTransform(context.Background(), v2, cfg)
	if len(session.transientConsumed) != 1 {
		t.Fatalf("expected 1 tracked consume (not the second non-promote); got %d", len(session.transientConsumed))
	}
}

// End-to-end: session rollback refunds every tracked consume via
// TransientBudget.Release so a fail-closed response doesn't burn the
// agent's one retry token for a recoverable verdict they never saw.
func TestPostprocessSession_RollbackRefundsConsumedTransientSlots(t *testing.T) {
	budget := llmproxy.NewMemoryTransientBudget(0)
	cfg := llmproxy.PostprocessConfig{
		AgentContext: llmproxy.AgentContext{AgentID: "agent-rollback", AgentUserID: "user-rollback"},
		AuditContext: llmproxy.AuditContext{ConversationID: "conv-rollback"},
		AuthorizationContext: llmproxy.AuthorizationContext{
			TransientBudget: budget,
		},
	}
	session := newPostprocessSession(cfg)

	tu := conversation.ToolUse{
		ID:    "tu-rollback",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl"}`),
	}
	v := conversation.TransientDenyVerdict("class-x", "transient")
	got := session.applyTransientTransform(context.Background(), v, cfg)
	if got.RecoverableReason == "" {
		t.Fatal("precondition: first call should promote and consume budget")
	}
	key := llmproxy.TransientBudgetKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		FailureClass:   "class-x",
	}
	if budget.Try(context.Background(), key) {
		t.Fatal("precondition: budget should be consumed after promote")
	}

	session.rollback(context.Background(), []conversation.ToolUse{tu}, map[string]conversation.ToolUseVerdict{tu.ID: got})

	if !budget.Try(context.Background(), key) {
		t.Fatal("after rollback, transient slot should be refunded so the next real attempt promotes")
	}
}
