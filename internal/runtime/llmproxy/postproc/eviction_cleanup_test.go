package postproc

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// TestCleanupEvictedInlineTask_NoOpWhenNoTaskID confirms the helper
// is a safe no-op when the evicted hold doesn't carry an inline-task
// link. Eviction can happen for any kind of approval hold; only the
// inline-task variety needs DB-side cleanup.
func TestCleanupEvictedInlineTask_NoOpWhenNoTaskID(t *testing.T) {
	ctx := context.Background()
	creator := &capturingInlineCreator{}
	llmproxy.CleanupEvictedInlineTask(ctx, llmproxy.PostprocessConfig{InlineTaskCreator: creator}, &llmproxy.PendingLiteApproval{
		ID:     "cv-tool-hold",
		UserID: "u",
	})
	if creator.expireCalled {
		t.Fatalf("ExpireInlineTask was called for a hold with no llmproxy.PendingTaskID")
	}
}

// TestCleanupEvictedInlineTask_NoOpWhenCreatorMissing confirms the
// helper does not panic when no creator is wired (or the creator
// doesn't implement the pending extension). Important because the
// eviction call sites fire on every Hold and must not crash when the
// daemon is wired in a partial / legacy shape.
func TestCleanupEvictedInlineTask_NoOpWhenCreatorMissing(t *testing.T) {
	ctx := context.Background()
	llmproxy.CleanupEvictedInlineTask(ctx, llmproxy.PostprocessConfig{}, &llmproxy.PendingLiteApproval{
		ID:            "cv-inline-hold",
		UserID:        "u",
		PendingTaskID: "task-aaa",
	})
}

// TestCleanupEvictedInlineTask_CallsExpireWhenInlineHold drives the
// happy path: an inline-task hold with a non-empty llmproxy.PendingTaskID
// triggers ExpireInlineTask on the configured creator. The cache
// eviction sites all funnel through this helper, so this is the only
// branch that needs end-to-end coverage of the cleanup wiring.
func TestCleanupEvictedInlineTask_CallsExpireWhenInlineHold(t *testing.T) {
	ctx := context.Background()
	creator := &capturingInlineCreator{}
	llmproxy.CleanupEvictedInlineTask(ctx, llmproxy.PostprocessConfig{InlineTaskCreator: creator}, &llmproxy.PendingLiteApproval{
		ID:            "cv-inline-hold",
		UserID:        "u",
		PendingTaskID: "task-aaa",
	})
	if !creator.expireCalled {
		t.Fatalf("ExpireInlineTask was not called for an inline-task hold")
	}
	if len(creator.expiredIDs) != 1 || creator.expiredIDs[0] != "task-aaa" {
		t.Fatalf("expired ids = %v, want [task-aaa]", creator.expiredIDs)
	}
}

func TestCleanupEvictedInlineTask_TracesExpireFailure(t *testing.T) {
	ctx := context.Background()
	creator := &capturingInlineCreator{expireFail: true}
	var buf bytes.Buffer
	llmproxy.CleanupEvictedInlineTask(ctx, llmproxy.PostprocessConfig{
		InlineTaskCreator: creator,
		RequestID:         "req-evict-trace",
		Trace:             llmproxy.NewTraceLogger(&buf),
	}, &llmproxy.PendingLiteApproval{
		ID:            "cv-inline-hold",
		UserID:        "u",
		AgentID:       "a",
		PendingTaskID: "task-aaa",
	})
	out := buf.String()
	if !strings.Contains(out, "inline_task.evicted_expire_failed") {
		t.Fatalf("trace missing eviction-expire failure event: %s", out)
	}
	if !strings.Contains(out, "task-aaa") {
		t.Fatalf("trace missing task id: %s", out)
	}
}

// TestReplayBufferedHolds_EvictionExpiresInlineTask drives the
// integration path the user reported: a buffered Hold that would
// displace an older inline-task hold during replay triggers
// ExpireInlineTask on the evicted hold's llmproxy.PendingTaskID so the
// dashboard doesn't strand a row whose chat anchor is gone.
func TestReplayBufferedHolds_EvictionExpiresInlineTask(t *testing.T) {
	ctx := context.Background()
	inner := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	// Force eviction on the very next Hold by tightening the cap.
	inner.SetMaxForTest(1)

	// Seed the inner cache with an inline-task hold that the replay
	// will evict — exactly the user's reported shape.
	if _, err := inner.Hold(ctx, llmproxy.PendingLiteApproval{
		ID:            "cv-inline-evicted",
		UserID:        "u",
		AgentID:       "a",
		Provider:      conversation.ProviderAnthropic,
		Stage:         llmproxy.StageAwaitingTaskApproval,
		PendingTaskID: "task-stranded",
		ToolUse:       conversation.ToolUse{ID: "tool_a", Name: "Bash"},
	}); err != nil {
		t.Fatalf("seed Hold: %v", err)
	}

	// Buffered new hold that will commit during replay. Without
	// inline-task linkage so we know any ExpireInlineTask call has
	// to be on the EVICTED hold, not this one.
	sink := &capturedHoldSink{
		holds: []capturedHold{{
			Pending: llmproxy.PendingLiteApproval{
				ID:       "cv-newcomer",
				UserID:   "u",
				AgentID:  "a",
				Provider: conversation.ProviderAnthropic,
				ToolUse:  conversation.ToolUse{ID: "tool_b", Name: "Bash"},
			},
		}},
	}
	creator := &capturingInlineCreator{}
	if err := replayBufferedHolds(ctx, llmproxy.PostprocessConfig{InlineTaskCreator: creator}, inner, sink, nil, nil); err != nil {
		t.Fatalf("replayBufferedHolds: %v", err)
	}

	if !creator.expireCalled {
		t.Fatalf("ExpireInlineTask was not invoked for the evicted inline-task hold")
	}
	if len(creator.expiredIDs) != 1 || creator.expiredIDs[0] != "task-stranded" {
		t.Fatalf("expired ids = %v, want [task-stranded] (llmproxy.PendingTaskID of the evicted hold)", creator.expiredIDs)
	}
}

func TestRollbackBufferedPendingTasks_ExpiresOnlyInlineTaskHolds(t *testing.T) {
	ctx := context.Background()
	creator := &capturingInlineCreator{}
	sink := &capturedHoldSink{
		holds: []capturedHold{
			{Pending: llmproxy.PendingLiteApproval{UserID: "u", PendingTaskID: "task-pending-1"}},
			{Pending: llmproxy.PendingLiteApproval{UserID: "u"}},
			{Pending: llmproxy.PendingLiteApproval{UserID: "u", PendingTaskID: "task-pending-2"}},
		},
	}

	rollbackBufferedPendingTasks(ctx, llmproxy.PostprocessConfig{InlineTaskCreator: creator}, sink)

	if !creator.expireCalled {
		t.Fatalf("ExpireInlineTask was not called for buffered inline-task holds")
	}
	want := []string{"task-pending-1", "task-pending-2"}
	if strings.Join(creator.expiredIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("expired ids = %v, want %v", creator.expiredIDs, want)
	}
	if creator.denyCalled {
		t.Fatalf("rollback should expire operational orphans, not deny them as user action")
	}
}

func TestRollbackBufferedPendingTasks_TracesExpireFailure(t *testing.T) {
	ctx := context.Background()
	creator := &capturingInlineCreator{expireFail: true}
	sink := &capturedHoldSink{
		holds: []capturedHold{{
			Pending: llmproxy.PendingLiteApproval{
				ID:            "cv-inline-rollback",
				UserID:        "u",
				AgentID:       "a",
				PendingTaskID: "task-rollback",
			},
		}},
	}
	var buf bytes.Buffer

	rollbackBufferedPendingTasks(ctx, llmproxy.PostprocessConfig{
		InlineTaskCreator: creator,
		RequestID:         "req-rollback-trace",
		Trace:             llmproxy.NewTraceLogger(&buf),
	}, sink)

	out := buf.String()
	if !strings.Contains(out, "inline_task.rollback_expire_failed") {
		t.Fatalf("trace missing rollback-expire failure event: %s", out)
	}
	if !strings.Contains(out, "task-rollback") {
		t.Fatalf("trace missing task id: %s", out)
	}
}
