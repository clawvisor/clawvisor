package postproc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// Shared fixture: a tool_use + base PostprocessConfig wired with an
// identity tuple and a fresh transient budget.
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

// Allow / Skip verdicts must pass through untouched.
func TestTransformTransientDenyToRecoverable_LeavesNonTransientAlone(t *testing.T) {
	_, tu, cfg := transientDenyTestSetup("any")
	verdict := conversation.ToolUseVerdict{Outcome: conversation.OutcomeAllow, Allowed: true}
	got := transformTransientDenyToRecoverable(context.Background(), verdict, tu, cfg, nil)
	if got.RecoverableReason != "" {
		t.Fatalf("non-transient verdict must not gain RecoverableReason; got %q", got.RecoverableReason)
	}
	if got.TransientFailureClass != "" {
		t.Fatalf("non-transient verdict must not gain TransientFailureClass; got %q", got.TransientFailureClass)
	}
}

// Plain Deny without TransientFailureClass must pass through.
func TestTransformTransientDenyToRecoverable_LeavesPlainDenyAlone(t *testing.T) {
	_, tu, cfg := transientDenyTestSetup("any")
	verdict := conversation.ToolUseVerdict{Outcome: conversation.OutcomeDeny, Reason: "plain"}
	got := transformTransientDenyToRecoverable(context.Background(), verdict, tu, cfg, nil)
	if got.RecoverableReason != "" {
		t.Fatalf("plain Deny must not be promoted; got RecoverableReason=%q", got.RecoverableReason)
	}
}

// If RecoverableReason is already set, the transient transform must not
// touch it — a prior layer chose the recoverable shape and re-running
// would be a double-process.
func TestTransformTransientDenyToRecoverable_LeavesRecoverableAlone(t *testing.T) {
	_, tu, cfg := transientDenyTestSetup("class")
	v := conversation.TransientDenyVerdict("class", "transient reason")
	v.RecoverableReason = "already recoverable"
	got := transformTransientDenyToRecoverable(context.Background(), v, tu, cfg, nil)
	if got.RecoverableReason != "already recoverable" {
		t.Fatalf("transform must not overwrite an existing RecoverableReason; got %q", got.RecoverableReason)
	}
}

// Missing identity tuple → safe degrade to plain Deny.
func TestTransformTransientDenyToRecoverable_RequiresIdentity(t *testing.T) {
	v, tu, cfg := transientDenyTestSetup("class")
	cfg.AgentContext.AgentID = ""
	got := transformTransientDenyToRecoverable(context.Background(), v, tu, cfg, nil)
	if got.RecoverableReason != "" {
		t.Fatalf("missing AgentID should NOT promote to recoverable; got %q", got.RecoverableReason)
	}

	v2, tu2, cfg2 := transientDenyTestSetup("class")
	cfg2.AuditContext.ConversationID = ""
	got2 := transformTransientDenyToRecoverable(context.Background(), v2, tu2, cfg2, nil)
	if got2.RecoverableReason != "" {
		t.Fatalf("missing ConversationID should NOT promote to recoverable; got %q", got2.RecoverableReason)
	}
}

// Nil budget → safe degrade to plain Deny.
func TestTransformTransientDenyToRecoverable_RequiresBudget(t *testing.T) {
	v, tu, cfg := transientDenyTestSetup("class")
	cfg.AuthorizationContext.TransientBudget = nil
	got := transformTransientDenyToRecoverable(context.Background(), v, tu, cfg, nil)
	if got.RecoverableReason != "" {
		t.Fatalf("nil budget should NOT promote to recoverable; got %q", got.RecoverableReason)
	}
}

// First call: budget is consumed and the verdict is promoted to
// RecoverableDeny shape (RecoverableReason populated, SubstituteWith
// populated, TransientFailureClass preserved for audit). Also asserts
// the tracker callback fires with the correct key — the session uses
// that to refund the slot on fail-closed rollback.
func TestTransformTransientDenyToRecoverable_FirstCallPromotesAndTracks(t *testing.T) {
	v, tu, cfg := transientDenyTestSetup("class-x")
	var tracked []llmproxy.TransientBudgetKey
	got := transformTransientDenyToRecoverable(context.Background(), v, tu, cfg, func(k llmproxy.TransientBudgetKey) {
		tracked = append(tracked, k)
	})
	if got.RecoverableReason != v.Reason {
		t.Fatalf("first call should set RecoverableReason to verdict reason; got %q, want %q", got.RecoverableReason, v.Reason)
	}
	if got.SubstituteWith != v.Reason {
		t.Fatalf("first call should set SubstituteWith to verdict reason; got %q", got.SubstituteWith)
	}
	if got.TransientFailureClass != "class-x" {
		t.Fatalf("TransientFailureClass should be preserved through the transform; got %q", got.TransientFailureClass)
	}
	want := llmproxy.TransientBudgetKey{
		AgentID:        cfg.AgentContext.AgentID,
		ConversationID: cfg.AuditContext.ConversationID,
		FailureClass:   "class-x",
	}
	if len(tracked) != 1 || tracked[0] != want {
		t.Fatalf("expected tracker to receive %+v exactly once; got %+v", want, tracked)
	}
}

// Tracker is only called when the budget was consumed — second call
// (budget exhausted) must not fire it. Pins the contract the session
// rollback relies on (refund list = actual consumes).
func TestTransformTransientDenyToRecoverable_TrackerSkippedOnBudgetExhausted(t *testing.T) {
	v, tu, cfg := transientDenyTestSetup("class-x")
	if got := transformTransientDenyToRecoverable(context.Background(), v, tu, cfg, nil); got.RecoverableReason == "" {
		t.Fatal("precondition: first call should promote")
	}
	var tracked []llmproxy.TransientBudgetKey
	v2 := conversation.TransientDenyVerdict("class-x", "retry reason")
	got := transformTransientDenyToRecoverable(context.Background(), v2, tu, cfg, func(k llmproxy.TransientBudgetKey) {
		tracked = append(tracked, k)
	})
	if got.RecoverableReason != "" {
		t.Fatalf("second call should not promote (budget exhausted); got %q", got.RecoverableReason)
	}
	if len(tracked) != 0 {
		t.Fatalf("tracker must not fire when Try returned false; got %+v", tracked)
	}
}

// Second call same (agent, conv, class): budget exhausted, verdict
// passes through as a plain Deny (RecoverableReason stays empty).
func TestTransformTransientDenyToRecoverable_SecondCallFallsThroughAsPlainDeny(t *testing.T) {
	v, tu, cfg := transientDenyTestSetup("class-x")
	first := transformTransientDenyToRecoverable(context.Background(), v, tu, cfg, nil)
	if first.RecoverableReason == "" {
		t.Fatal("first call should promote (precondition)")
	}
	// Build a fresh verdict for the retry — the upstream evaluator
	// produces a new one each time.
	v2 := conversation.TransientDenyVerdict("class-x", "retry reason")
	got := transformTransientDenyToRecoverable(context.Background(), v2, tu, cfg, nil)
	if got.RecoverableReason != "" {
		t.Fatalf("second call should not promote (budget exhausted); got RecoverableReason=%q", got.RecoverableReason)
	}
	if got.Outcome != conversation.OutcomeDeny {
		t.Fatalf("second call should remain Deny; got %v", got.Outcome)
	}
}

// Different failure class on the same (agent, conv): independent budget.
func TestTransformTransientDenyToRecoverable_DistinctClassesIndependent(t *testing.T) {
	_, tu, cfg := transientDenyTestSetup("class-a")
	vA := conversation.TransientDenyVerdict("class-a", "reason A")
	gotA := transformTransientDenyToRecoverable(context.Background(), vA, tu, cfg, nil)
	if gotA.RecoverableReason == "" {
		t.Fatal("class-a first call should promote")
	}
	vB := conversation.TransientDenyVerdict("class-b", "reason B")
	gotB := transformTransientDenyToRecoverable(context.Background(), vB, tu, cfg, nil)
	if gotB.RecoverableReason == "" {
		t.Fatal("class-b first call should promote independently of class-a")
	}
}

// Session rollback must refund every consumed transient slot via
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
	got := transformTransientDenyToRecoverable(context.Background(), v, tu, cfg, session.trackTransientConsumed)
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

	// Simulate fail-closed: rollback runs.
	session.rollback(context.Background(), []conversation.ToolUse{tu}, map[string]conversation.ToolUseVerdict{tu.ID: got})

	// Slot is refunded — a subsequent attempt for the same key should
	// succeed (it's the agent's first real retry).
	if !budget.Try(context.Background(), key) {
		t.Fatal("after rollback, transient budget slot should be refunded so the next real attempt promotes")
	}
}

// Different conversation on the same class: independent budget.
func TestTransformTransientDenyToRecoverable_DistinctConversationsIndependent(t *testing.T) {
	v1, tu1, cfg1 := transientDenyTestSetup("class-x")
	got1 := transformTransientDenyToRecoverable(context.Background(), v1, tu1, cfg1, nil)
	if got1.RecoverableReason == "" {
		t.Fatal("conv-1 first call should promote")
	}
	v2, tu2, cfg2 := transientDenyTestSetup("class-x")
	cfg2.AuditContext.ConversationID = "conv-different"
	// Reuse cfg1's budget so we're testing key-by-conversation,
	// not budget isolation.
	cfg2.AuthorizationContext.TransientBudget = cfg1.AuthorizationContext.TransientBudget
	got2 := transformTransientDenyToRecoverable(context.Background(), v2, tu2, cfg2, nil)
	if got2.RecoverableReason == "" {
		t.Fatal("conv-2 first call should promote even though conv-1 already consumed its budget")
	}
}
