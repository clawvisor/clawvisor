package postproc

import (
	"context"
	"errors"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
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

	if err := s.commitSubstitutions(context.Background(), verdictByTU, toolUses); err != nil {
		t.Fatalf("commitSubstitutions: %v", err)
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
	s.rollbackSubstitutions(context.Background())
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

	err := s.commitSubstitutions(context.Background(), verdictByTU, []conversation.ToolUse{tu})
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
