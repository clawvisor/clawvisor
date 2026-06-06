package pipeline_test

import (
	"context"
	"errors"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/eval"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

type finalizerTestDeps struct {
	submit pipeline.HoldSubmitResult
	audits []conversation.AuditEvent
}

func (d *finalizerTestDeps) SubmitHold(context.Context, any) (pipeline.HoldSubmitResult, error) {
	return d.submit, nil
}

func (d *finalizerTestDeps) DropHold(context.Context, pipeline.HoldCapture) {}

func (d *finalizerTestDeps) BuildCoalescedHold([]pipeline.HoldCapture) pipeline.CoalescedHold {
	return pipeline.CoalescedHold{
		Payload: "coalesced",
		EvictedAuditFor: func(_ pipeline.HoldCapture, evictedID string) conversation.AuditEvent {
			return conversation.AuditEvent{
				OutcomeName: "approval_evicted",
				Reason:      evictedID,
			}
		},
		PerToolAuditFor: func(_ pipeline.HoldCapture, approvalID string) conversation.AuditEvent {
			return conversation.AuditEvent{
				OutcomeName: "coalesced_approval_pending",
				Reason:      approvalID,
			}
		},
		Prompt: func(string) string { return "approval required" },
	}
}

func (d *finalizerTestDeps) BuildReplayFailedAudit(pipeline.HoldCapture, error) conversation.AuditEvent {
	return conversation.AuditEvent{OutcomeName: "approval_hold_replay_failed"}
}

func (d *finalizerTestDeps) BuildEvictedAudit(_ pipeline.HoldCapture, evictedID string) conversation.AuditEvent {
	return conversation.AuditEvent{
		OutcomeName: "approval_evicted",
		Reason:      evictedID,
	}
}

func (d *finalizerTestDeps) CleanupEvictedHold(context.Context, any) {}

func (d *finalizerTestDeps) RollbackPendingTask(context.Context, pipeline.HoldCapture) {}

func (d *finalizerTestDeps) WriteAudit(_ context.Context, ev conversation.AuditEvent) {
	d.audits = append(d.audits, ev)
}

func TestFinalizerReplayEvictionAuditsEvictedApprovalID(t *testing.T) {
	deps := &finalizerTestDeps{
		submit: pipeline.HoldSubmitResult{
			ApprovalID:        "cv-new",
			EvictedApprovalID: "cv-old",
			Evicted:           errors.New("opaque evicted payload"),
		},
	}
	f := pipeline.NewFinalizer(deps)
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_1",
		Kind:      eval.HeldKindHintApproval,
		Payload:   "pending",
	})

	if _, err := f.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if len(deps.audits) != 1 {
		t.Fatalf("audit count = %d, want 1: %+v", len(deps.audits), deps.audits)
	}
	if got := deps.audits[0].Reason; got != "cv-old" {
		t.Fatalf("evicted audit ID = %q, want cv-old", got)
	}
}

func TestFinalizerCoalescedEvictionAuditsEvictedApprovalID(t *testing.T) {
	deps := &finalizerTestDeps{
		submit: pipeline.HoldSubmitResult{
			ApprovalID:        "cv-new",
			EvictedApprovalID: "cv-old",
			Evicted:           errors.New("opaque evicted payload"),
		},
	}
	f := pipeline.NewFinalizer(deps)
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_1",
		Kind:      eval.HeldKindHintApproval,
		Payload:   "pending",
	})
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_2",
		Kind:      eval.HeldKindHintAllow,
	})

	if _, err := f.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if len(deps.audits) < 1 {
		t.Fatalf("audit count = %d, want at least 1", len(deps.audits))
	}
	if got := deps.audits[0].Reason; got != "cv-old" {
		t.Fatalf("evicted audit ID = %q, want cv-old", got)
	}
}
