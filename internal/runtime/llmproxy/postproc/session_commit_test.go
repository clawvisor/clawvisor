package postproc

import (
	"context"
	"errors"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/taskcheckout"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestCommitSubstitutionsRegistersSpecsInToolUseOrder pins the
// spec-on-verdict invariant: evaluators MUST attach a
// PendingSubstitutionSpec to the verdict and MUST NOT touch the
// registry themselves; commitSubstitutions is the single point that
// translates specs into registry writes, walking decisions in
// tool_use order so the audit + rollback layers see a deterministic
// sequence.
func TestCommitSubstitutionsRegistersSpecsInToolUseOrder(t *testing.T) {
	reg := llmproxy.NewMemoryScopeDriftRegistry(0)
	cfg := llmproxy.PostprocessConfig{
		AgentContext:         llmproxy.AgentContext{AgentID: "agent-commit", AgentUserID: "user-commit"},
		AuditContext:         llmproxy.AuditContext{ConversationID: "conv-commit"},
		AuthorizationContext: llmproxy.AuthorizationContext{ScopeDrifts: reg},
	}
	s := newPostprocessSession(cfg)

	toolUses := []conversation.ToolUse{
		{ID: "tu-1", Name: "Bash", Input: []byte(`{"command":"a"}`)},
		{ID: "tu-2", Name: "Bash", Input: []byte(`{"command":"b"}`)},
	}
	verdictByTU := map[string]conversation.ToolUseVerdict{
		"tu-1": {
			Outcome: conversation.OutcomeDeny,
			PendingSubstitution: &conversation.PendingSubstitutionSpec{
				DriftID:           "drift-1",
				MenuText:          "first menu",
				OriginalToolName:  "Write",
				OriginalToolInput: []byte(`{"path":"a"}`),
			},
		},
		"tu-2": {
			Outcome: conversation.OutcomeDeny,
			PendingSubstitution: &conversation.PendingSubstitutionSpec{
				DriftID:           "drift-2",
				MenuText:          "second menu",
				OriginalToolName:  "Edit",
				OriginalToolInput: []byte(`{"path":"b"}`),
			},
		},
	}

	if err := s.commitVerdictSideEffects(context.Background(), verdictByTU, toolUses); err != nil {
		t.Fatalf("commitVerdictSideEffects: %v", err)
	}

	for _, tu := range toolUses {
		got, ok := reg.LookupPendingSubstitution(context.Background(), llmproxy.PendingSubstitutionKey{
			AgentID:        cfg.AgentContext.AgentID,
			ConversationID: cfg.AuditContext.ConversationID,
			ToolUseID:      tu.ID,
		})
		if !ok {
			t.Fatalf("expected substitution registered for %s", tu.ID)
		}
		wantText := "first menu"
		if tu.ID == "tu-2" {
			wantText = "second menu"
		}
		if got.MenuText != wantText {
			t.Fatalf("%s MenuText = %q; want %q", tu.ID, got.MenuText, wantText)
		}
	}

	// The session's tracked keys feed rollback. Confirm both keys
	// landed so a later failClosed sweeps everything.
	if len(s.substitutions) != 2 {
		t.Fatalf("expected 2 tracked keys, got %d", len(s.substitutions))
	}

	// Round-trip: rollback deletes both.
	s.rollbackVerdictSideEffects(context.Background())
	for _, tu := range toolUses {
		if _, ok := reg.LookupPendingSubstitution(context.Background(), llmproxy.PendingSubstitutionKey{
			AgentID:        cfg.AgentContext.AgentID,
			ConversationID: cfg.AuditContext.ConversationID,
			ToolUseID:      tu.ID,
		}); ok {
			t.Fatalf("rollback left %s registered", tu.ID)
		}
	}
}

// TestCommitSubstitutionsExpiresTaskOnRegistrationFailure asserts the
// post-create rollback contract the auto-approve path relies on: when
// the registry write fails AFTER an inline task was already created,
// commitSubstitutions invokes ExpireInlineApprovedTask via the
// configured InlineApprovedTaskExpirer so the dashboard doesn't
// strand an orphan, and propagates the registry error so the caller
// can fail-closed the response.
func TestCommitSubstitutionsExpiresTaskOnRegistrationFailure(t *testing.T) {
	reg := &commitFailingRegistry{inner: llmproxy.NewMemoryScopeDriftRegistry(0)}
	expirer := &recordingExpirer{}
	cfg := llmproxy.PostprocessConfig{
		AgentContext:         llmproxy.AgentContext{AgentID: "agent-cmt", AgentUserID: "user-cmt"},
		AuditContext:         llmproxy.AuditContext{ConversationID: "conv-cmt"},
		AuthorizationContext: llmproxy.AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext:      llmproxy.ApprovalContext{InlineTaskCreator: expirer},
	}
	s := newPostprocessSession(cfg)

	tu := conversation.ToolUse{ID: "tu-fail", Name: "Bash", Input: []byte(`{"command":"x"}`)}
	verdictByTU := map[string]conversation.ToolUseVerdict{
		"tu-fail": {
			Outcome: conversation.OutcomeDeny,
			PendingSubstitution: &conversation.PendingSubstitutionSpec{
				MenuText:          "augmentation",
				OriginalToolName:  tu.Name,
				OriginalToolInput: tu.Input,
				TaskRollback: &conversation.PendingSubstitutionTaskRollback{
					TaskID: "task-orphan",
					UserID: "user-cmt",
				},
			},
		},
	}

	err := s.commitVerdictSideEffects(context.Background(), verdictByTU, []conversation.ToolUse{tu})
	if err == nil {
		t.Fatal("expected error propagated to caller for failClosed")
	}
	if !errors.Is(err, errCommitForcedFailure) {
		t.Fatalf("expected wrapped registry error, got %v", err)
	}
	if !expirer.expireCalled {
		t.Fatal("expected InlineApprovedTaskExpirer.ExpireInlineApprovedTask to fire after registry failure")
	}
	if expirer.expiredTaskID != "task-orphan" || expirer.expiredUserID != "user-cmt" {
		t.Fatalf("expirer called with wrong identity: task=%q user=%q",
			expirer.expiredTaskID, expirer.expiredUserID)
	}
	if len(s.substitutions) != 0 {
		t.Fatalf("expected no tracked keys on failure (rollback already handled), got %d", len(s.substitutions))
	}
}

// TestCommitVerdictSideEffectsAppliesDriftOutcomeBeforeSubstitution
// pins the per-verdict ordering: SetOutcome(Succeeded) lands before
// RegisterPendingSubstitution so the pre-clear is in place for any
// LookupPreClear that follows during the same request flow. Also
// verifies rollback unwinds both: drift outcome (via RollbackClaim,
// which deletes the pre-clear) AND the substitution.
func TestCommitVerdictSideEffectsAppliesDriftOutcomeBeforeSubstitution(t *testing.T) {
	reg := llmproxy.NewMemoryScopeDriftRegistry(0)
	ctx := context.Background()
	// Seed a claimed drift so SetOutcome has a real target. The
	// fingerprint feeds LookupPreClear after commit.
	tu := conversation.ToolUse{ID: "tu-defer", Name: "Bash", Input: []byte(`{"command":"x"}`)}
	stored, err := reg.Register(ctx, llmproxy.ScopeDrift{
		UserID:         "user-defer",
		AgentID:        "agent-defer",
		ConversationID: "conv-defer",
		ToolUse:        tu,
		Source:         llmproxy.ScopeDriftSourceTaskScope,
	})
	if err != nil {
		t.Fatalf("Register drift: %v", err)
	}
	if _, err := reg.ClaimOption(ctx, stored.ID, llmproxy.ScopeDriftOptionNewTask, ""); err != nil {
		t.Fatalf("ClaimOption: %v", err)
	}

	cfg := llmproxy.PostprocessConfig{
		AgentContext:         llmproxy.AgentContext{AgentID: "agent-defer", AgentUserID: "user-defer"},
		AuditContext:         llmproxy.AuditContext{ConversationID: "conv-defer"},
		AuthorizationContext: llmproxy.AuthorizationContext{ScopeDrifts: reg},
	}
	s := newPostprocessSession(cfg)

	verdictByTU := map[string]conversation.ToolUseVerdict{
		"tu-defer": {
			Outcome: conversation.OutcomeDeny,
			DeferredDriftOutcome: &conversation.DeferredDriftOutcomeSpec{
				DriftID: stored.ID,
				Outcome: string(llmproxy.ScopeDriftOutcomeSucceeded),
			},
			PendingSubstitution: &conversation.PendingSubstitutionSpec{
				DriftID:           stored.ID,
				MenuText:          "augmentation",
				OriginalToolName:  tu.Name,
				OriginalToolInput: tu.Input,
			},
		},
	}

	if err := s.commitVerdictSideEffects(ctx, verdictByTU, []conversation.ToolUse{tu}); err != nil {
		t.Fatalf("commitVerdictSideEffects: %v", err)
	}

	// Drift outcome landed → pre-clear minted, LookupPreClear hits.
	driftID, hit := reg.LookupPreClear(ctx, "agent-defer", stored.Fingerprint())
	if !hit || driftID != stored.ID {
		t.Fatalf("LookupPreClear after commit: ok=%v id=%q want id=%q", hit, driftID, stored.ID)
	}

	// Substitution landed too.
	if _, ok := reg.LookupPendingSubstitution(ctx, llmproxy.PendingSubstitutionKey{
		AgentID:        "agent-defer",
		ConversationID: "conv-defer",
		ToolUseID:      "tu-defer",
	}); !ok {
		t.Fatal("expected substitution registered")
	}

	// Rollback wipes both: substitution gone, and the drift outcome's
	// pre-clear is gone too (RollbackClaim resets + deletes pre-clear).
	// LookupPreClear above already consumed the entry; mint another by
	// re-running SetOutcome and confirming rollback removes it.
	if err := reg.SetOutcome(ctx, stored.ID, llmproxy.ScopeDriftOutcomeSucceeded); err != nil {
		t.Fatalf("re-SetOutcome: %v", err)
	}
	s.driftOutcomes = []string{stored.ID}
	s.substitutions = []llmproxy.PendingSubstitutionKey{{
		AgentID: "agent-defer", ConversationID: "conv-defer", ToolUseID: "tu-defer",
	}}
	// Re-register the substitution so rollback has something to delete.
	_ = reg.RegisterPendingSubstitution(ctx, s.substitutions[0], llmproxy.PendingSubstitution{
		MenuText: "augmentation", OriginalToolName: tu.Name, OriginalToolInput: tu.Input,
	})

	s.rollbackVerdictSideEffects(ctx)

	if _, hit := reg.LookupPreClear(ctx, "agent-defer", stored.Fingerprint()); hit {
		t.Fatal("rollback should have deleted the pre-clear via RollbackClaim")
	}
	if _, ok := reg.LookupPendingSubstitution(ctx, llmproxy.PendingSubstitutionKey{
		AgentID: "agent-defer", ConversationID: "conv-defer", ToolUseID: "tu-defer",
	}); ok {
		t.Fatal("rollback should have deleted the substitution")
	}
}

// TestCommitVerdictSideEffectsRollsBackClaimOnSetOutcomeFailure
// covers the auto-approve recovery contract: if the evaluator
// emitted DeferredDriftOutcome + PendingSubstitution but the
// SetOutcome write fails, the drift claim is rolled back (so the
// agent's retry mints fresh instead of seeing a claimed-but-
// unresolved drift) AND the orphan task is expired via the spec's
// TaskRollback.
//
// This test moved from internal/runtime/llmproxy/scope_drift_e2e_test.go
// after Gap-1: the SetOutcome side-effect lives in postproc now, so
// the rollback test belongs alongside the layer that owns the behavior.
func TestCommitVerdictSideEffectsRollsBackClaimOnSetOutcomeFailure(t *testing.T) {
	innerReg := llmproxy.NewMemoryScopeDriftRegistry(0)
	reg := &failingSetOutcomeRegistry{ScopeDriftRegistry: innerReg}
	ctx := context.Background()

	// Seed a claimed (Pending) drift, mimicking the state guard.Claim
	// leaves the registry in after the evaluator returns its verdict.
	tu := conversation.ToolUse{ID: "tu-setout", Name: "Bash", Input: []byte(`{"command":"x"}`)}
	stored, err := reg.Register(ctx, llmproxy.ScopeDrift{
		UserID:         "user-rb",
		AgentID:        "agent-rb",
		ConversationID: "conv-rb",
		ToolUse:        tu,
		Source:         llmproxy.ScopeDriftSourceTaskScope,
	})
	if err != nil {
		t.Fatalf("Register drift: %v", err)
	}
	if _, err := reg.ClaimOption(ctx, stored.ID, llmproxy.ScopeDriftOptionNewTask, ""); err != nil {
		t.Fatalf("ClaimOption: %v", err)
	}

	expirer := &recordingExpirer{}
	cfg := llmproxy.PostprocessConfig{
		AgentContext:         llmproxy.AgentContext{AgentID: "agent-rb", AgentUserID: "user-rb"},
		AuditContext:         llmproxy.AuditContext{ConversationID: "conv-rb"},
		AuthorizationContext: llmproxy.AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext:      llmproxy.ApprovalContext{InlineTaskCreator: expirer},
	}
	s := newPostprocessSession(cfg)

	verdictByTU := map[string]conversation.ToolUseVerdict{
		"tu-setout": {
			Outcome: conversation.OutcomeDeny,
			DeferredDriftOutcome: &conversation.DeferredDriftOutcomeSpec{
				DriftID: stored.ID,
				Outcome: string(llmproxy.ScopeDriftOutcomeSucceeded),
			},
			PendingSubstitution: &conversation.PendingSubstitutionSpec{
				DriftID:           stored.ID,
				MenuText:          "augmentation",
				OriginalToolName:  tu.Name,
				OriginalToolInput: tu.Input,
				TaskRollback: &conversation.PendingSubstitutionTaskRollback{
					TaskID: "task-orphan",
					UserID: "user-rb",
				},
			},
		},
	}

	if err := s.commitVerdictSideEffects(ctx, verdictByTU, []conversation.ToolUse{tu}); err == nil {
		t.Fatal("expected SetOutcome failure to propagate so the caller fail-closes")
	}
	if !expirer.expireCalled {
		t.Fatal("expected TaskRollback expirer to fire on SetOutcome failure")
	}

	// Claim must have been rolled back so the agent's retry can mint
	// fresh. RollbackClaim resets ChosenOption and Outcome to empty.
	rolled, err := reg.Get(ctx, stored.ID)
	if err != nil {
		t.Fatalf("Get drift: %v", err)
	}
	if rolled.ChosenOption != "" {
		t.Fatalf("drift ChosenOption = %q, want empty after rollback", rolled.ChosenOption)
	}
	if rolled.Outcome != "" {
		t.Fatalf("drift Outcome = %q, want empty after rollback", rolled.Outcome)
	}
	// And the claim is reusable.
	if _, err := reg.ClaimOption(ctx, stored.ID, llmproxy.ScopeDriftOptionNewTask, "retry"); err != nil {
		t.Fatalf("expected drift to be re-claimable after rollback, got: %v", err)
	}
}

// TestCommitFailureClearsAutoApproveCheckout pins the auto-approve
// checkout-rollback contract: when the deferred-outcome / substitution
// commit fails, the conversation checkout the evaluator set inline
// before returning the verdict gets cleared too. Without that sweep
// the next turn surfaces a "task missing" experience — model fetches
// the active task, sees the just-expired ID, asks the user to retry.
//
// Conditional clear: only when the stored value still names OUR task,
// so a concurrent flow that re-pointed the checkout after we set it
// isn't clobbered.
func TestCommitFailureClearsAutoApproveCheckout(t *testing.T) {
	reg := &commitFailingRegistry{inner: llmproxy.NewMemoryScopeDriftRegistry(0)}
	checkouts := taskcheckout.NewMemoryStore(0)
	expirer := &recordingExpirer{}
	cfg := llmproxy.PostprocessConfig{
		AgentContext:         llmproxy.AgentContext{AgentID: "agent-co", AgentUserID: "user-co"},
		AuditContext:         llmproxy.AuditContext{ConversationID: "conv-co"},
		AuthorizationContext: llmproxy.AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext:      llmproxy.ApprovalContext{InlineTaskCreator: expirer, Checkouts: checkouts},
	}
	s := newPostprocessSession(cfg)
	ctx := context.Background()

	// Simulate the auto-approve evaluator having set the checkout
	// to its just-created task BEFORE returning the verdict.
	key := taskcheckout.Key{UserID: "user-co", AgentID: "agent-co", ConversationID: "conv-co"}
	if err := checkouts.Set(ctx, key, "task-orphan", 0); err != nil {
		t.Fatalf("seed checkout: %v", err)
	}

	tu := conversation.ToolUse{ID: "tu-co", Name: "Bash", Input: []byte(`{"command":"x"}`)}
	verdictByTU := map[string]conversation.ToolUseVerdict{
		"tu-co": {
			Outcome: conversation.OutcomeDeny,
			PendingSubstitution: &conversation.PendingSubstitutionSpec{
				MenuText:          "augmentation",
				OriginalToolName:  tu.Name,
				OriginalToolInput: tu.Input,
				TaskRollback: &conversation.PendingSubstitutionTaskRollback{
					TaskID:         "task-orphan",
					UserID:         "user-co",
					AgentID:        "agent-co",
					ConversationID: "conv-co",
				},
			},
		},
	}

	if err := s.commitVerdictSideEffects(ctx, verdictByTU, []conversation.ToolUse{tu}); err == nil {
		t.Fatal("expected registry failure to propagate")
	}
	if !expirer.expireCalled {
		t.Fatal("task expirer should fire on commit failure")
	}
	// Checkout still points at the orphan task without our fix; with
	// the fix it's gone.
	if _, ok, err := checkouts.Get(ctx, key); err != nil {
		t.Fatalf("Get checkout: %v", err)
	} else if ok {
		t.Fatal("checkout pointing at orphan task should have been cleared")
	}
}

// TestCommitFailureDoesNotClobberReassignedCheckout asserts the
// conditional-clear contract: if a concurrent flow re-pointed the
// checkout between Set and rollback, the rollback must leave the new
// value alone.
func TestCommitFailureDoesNotClobberReassignedCheckout(t *testing.T) {
	reg := &commitFailingRegistry{inner: llmproxy.NewMemoryScopeDriftRegistry(0)}
	checkouts := taskcheckout.NewMemoryStore(0)
	expirer := &recordingExpirer{}
	cfg := llmproxy.PostprocessConfig{
		AgentContext:         llmproxy.AgentContext{AgentID: "agent-co2", AgentUserID: "user-co2"},
		AuditContext:         llmproxy.AuditContext{ConversationID: "conv-co2"},
		AuthorizationContext: llmproxy.AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext:      llmproxy.ApprovalContext{InlineTaskCreator: expirer, Checkouts: checkouts},
	}
	s := newPostprocessSession(cfg)
	ctx := context.Background()

	key := taskcheckout.Key{UserID: "user-co2", AgentID: "agent-co2", ConversationID: "conv-co2"}
	// Concurrent flow pointed the checkout at a DIFFERENT task after
	// our Set but before our rollback. The orphan-task expirer should
	// still fire, but the checkout's new value must survive.
	if err := checkouts.Set(ctx, key, "task-other-flow", 0); err != nil {
		t.Fatalf("seed checkout: %v", err)
	}

	tu := conversation.ToolUse{ID: "tu-co2", Name: "Bash", Input: []byte(`{"command":"x"}`)}
	verdictByTU := map[string]conversation.ToolUseVerdict{
		"tu-co2": {
			Outcome: conversation.OutcomeDeny,
			PendingSubstitution: &conversation.PendingSubstitutionSpec{
				MenuText:          "augmentation",
				OriginalToolName:  tu.Name,
				OriginalToolInput: tu.Input,
				TaskRollback: &conversation.PendingSubstitutionTaskRollback{
					TaskID:         "task-orphan",
					UserID:         "user-co2",
					AgentID:        "agent-co2",
					ConversationID: "conv-co2",
				},
			},
		},
	}

	_ = s.commitVerdictSideEffects(ctx, verdictByTU, []conversation.ToolUse{tu})

	current, ok, err := checkouts.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get checkout: %v", err)
	}
	if !ok || current.TaskID != "task-other-flow" {
		t.Fatalf("checkout reassigned by concurrent flow should survive rollback; got ok=%v task=%q", ok, current.TaskID)
	}
}

// TestSessionDropAndRollbackSweepsVerdictSideEffects pins the
// streaming-path rollback contract: the helpers that the streaming
// write paths call on a write failure (dropCommittedAndRollback,
// dropAllCommittedAndRollback) MUST also undo registry writes
// commitVerdictSideEffects landed earlier. Without this, a streaming
// commit-then-write-fail leaves stale substitutions / drift outcomes
// the harness retry would hit.
func TestSessionDropAndRollbackSweepsVerdictSideEffects(t *testing.T) {
	reg := llmproxy.NewMemoryScopeDriftRegistry(0)
	ctx := context.Background()
	cfg := llmproxy.PostprocessConfig{
		AgentContext:         llmproxy.AgentContext{AgentID: "agent-stream", AgentUserID: "user-stream"},
		AuditContext:         llmproxy.AuditContext{ConversationID: "conv-stream"},
		AuthorizationContext: llmproxy.AuthorizationContext{ScopeDrifts: reg},
	}
	s := newPostprocessSession(cfg)

	// Pretend commit landed two substitutions and one drift outcome.
	stored, err := reg.Register(ctx, llmproxy.ScopeDrift{
		UserID: "user-stream", AgentID: "agent-stream", ConversationID: "conv-stream",
		ToolUse: conversation.ToolUse{ID: "tu-1"}, Source: llmproxy.ScopeDriftSourceTaskScope,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := reg.ClaimOption(ctx, stored.ID, llmproxy.ScopeDriftOptionNewTask, ""); err != nil {
		t.Fatalf("ClaimOption: %v", err)
	}
	if err := reg.SetOutcome(ctx, stored.ID, llmproxy.ScopeDriftOutcomeSucceeded); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	key := llmproxy.PendingSubstitutionKey{
		AgentID: "agent-stream", ConversationID: "conv-stream", ToolUseID: "tu-1",
	}
	if err := reg.RegisterPendingSubstitution(ctx, key, llmproxy.PendingSubstitution{
		MenuText: "menu", OriginalToolName: "Bash", OriginalToolInput: []byte(`{}`),
	}); err != nil {
		t.Fatalf("RegisterPendingSubstitution: %v", err)
	}
	s.substitutions = []llmproxy.PendingSubstitutionKey{key}
	s.driftOutcomes = []string{stored.ID}

	if err := s.dropAllCommittedAndRollback(ctx); err != nil {
		t.Fatalf("dropAllCommittedAndRollback: %v", err)
	}

	if _, ok := reg.LookupPendingSubstitution(ctx, key); ok {
		t.Fatal("dropAllCommittedAndRollback must sweep verdict side effects: substitution still present")
	}
	if _, hit := reg.LookupPreClear(ctx, "agent-stream", stored.Fingerprint()); hit {
		t.Fatal("dropAllCommittedAndRollback must sweep drift outcomes: pre-clear still present")
	}
}

type failingSetOutcomeRegistry struct {
	llmproxy.ScopeDriftRegistry
}

func (r *failingSetOutcomeRegistry) SetOutcome(ctx context.Context, driftID string, outcome llmproxy.ScopeDriftOutcome) error {
	return errCommitForcedFailure
}

var errCommitForcedFailure = errors.New("forced registry failure")

type commitFailingRegistry struct {
	inner llmproxy.ScopeDriftRegistry
}

func (r *commitFailingRegistry) Register(ctx context.Context, drift llmproxy.ScopeDrift) (llmproxy.ScopeDrift, error) {
	return r.inner.Register(ctx, drift)
}

func (r *commitFailingRegistry) Get(ctx context.Context, driftID string) (llmproxy.ScopeDrift, error) {
	return r.inner.Get(ctx, driftID)
}

func (r *commitFailingRegistry) ClaimOption(ctx context.Context, driftID string, option llmproxy.ScopeDriftOption, agentNote string) (llmproxy.ScopeDrift, error) {
	return r.inner.ClaimOption(ctx, driftID, option, agentNote)
}

func (r *commitFailingRegistry) RollbackClaim(ctx context.Context, driftID string) error {
	return r.inner.RollbackClaim(ctx, driftID)
}

func (r *commitFailingRegistry) SetOutcome(ctx context.Context, driftID string, outcome llmproxy.ScopeDriftOutcome) error {
	return r.inner.SetOutcome(ctx, driftID, outcome)
}

func (r *commitFailingRegistry) LookupPreClear(ctx context.Context, agentID, fingerprint string) (string, bool) {
	return r.inner.LookupPreClear(ctx, agentID, fingerprint)
}

func (r *commitFailingRegistry) RegisterPendingSubstitution(ctx context.Context, key llmproxy.PendingSubstitutionKey, value llmproxy.PendingSubstitution) error {
	return errCommitForcedFailure
}

func (r *commitFailingRegistry) LookupPendingSubstitution(ctx context.Context, key llmproxy.PendingSubstitutionKey) (llmproxy.PendingSubstitution, bool) {
	return r.inner.LookupPendingSubstitution(ctx, key)
}

func (r *commitFailingRegistry) DeletePendingSubstitution(ctx context.Context, key llmproxy.PendingSubstitutionKey) {
	r.inner.DeletePendingSubstitution(ctx, key)
}

type recordingExpirer struct {
	expireCalled  bool
	expiredTaskID string
	expiredUserID string
}

func (r *recordingExpirer) CreateInlineApprovedTask(_ context.Context, _ *store.Agent, _ *runtimetasks.TaskCreateRequest, _ string) (*llmproxy.InlineApprovedTask, error) {
	return nil, errors.New("not used in this test")
}

func (r *recordingExpirer) ExpireInlineApprovedTask(_ context.Context, taskID, userID string) error {
	r.expireCalled = true
	r.expiredTaskID = taskID
	r.expiredUserID = userID
	return nil
}
