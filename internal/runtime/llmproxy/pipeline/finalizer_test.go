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
	submit                         pipeline.HoldSubmitResult
	submitErrs                     []error
	submitCalls                    int
	dropErr                        error
	dropped                        []pipeline.HoldCapture
	audits                         []conversation.AuditEvent
	coalescedEvictedAuditToolUseID []string
}

func (d *finalizerTestDeps) SubmitHold(context.Context, any) (pipeline.HoldSubmitResult, error) {
	d.submitCalls++
	if len(d.submitErrs) >= d.submitCalls && d.submitErrs[d.submitCalls-1] != nil {
		return pipeline.HoldSubmitResult{}, d.submitErrs[d.submitCalls-1]
	}
	return d.submit, nil
}

func (d *finalizerTestDeps) DropHold(_ context.Context, c pipeline.HoldCapture) error {
	d.dropped = append(d.dropped, c)
	return d.dropErr
}

func (d *finalizerTestDeps) BuildCoalescedHold([]pipeline.HoldCapture) pipeline.CoalescedHold {
	return pipeline.CoalescedHold{
		Payload: "coalesced",
		EvictedAuditFor: func(c pipeline.HoldCapture, evictedID string) conversation.AuditEvent {
			d.coalescedEvictedAuditToolUseID = append(d.coalescedEvictedAuditToolUseID, c.ToolUseID)
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

func TestFinalizerCoalescedReplacesBufferedAudits(t *testing.T) {
	deps := &finalizerTestDeps{
		submit: pipeline.HoldSubmitResult{ApprovalID: "cv-coalesced"},
	}
	f := pipeline.NewFinalizer(deps)
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_hold",
		Kind:      eval.HeldKindHintApproval,
		Payload:   "pending",
	})
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_allow",
		Kind:      eval.HeldKindHintAllow,
	})
	f.AddAudit(conversation.AuditEvent{OutcomeName: "approval_pending"})
	f.AddAudit(conversation.AuditEvent{OutcomeName: "allow"})

	if _, err := f.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if len(deps.audits) != 2 {
		t.Fatalf("audit count = %d, want 2 coalesced rows: %+v", len(deps.audits), deps.audits)
	}
	for _, ev := range deps.audits {
		if ev.OutcomeName != "coalesced_approval_pending" {
			t.Fatalf("unexpected buffered audit leaked on coalesce path: %+v", deps.audits)
		}
	}
}

func TestFinalizerReplayFailureReturnsDropError(t *testing.T) {
	submitErr := errors.New("submit failed")
	dropErr := errors.New("drop failed")
	deps := &finalizerTestDeps{
		submit:     pipeline.HoldSubmitResult{ApprovalID: "cv-1"},
		submitErrs: []error{nil, submitErr},
		dropErr:    dropErr,
	}
	f := pipeline.NewFinalizer(deps)
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_committed",
		Kind:      eval.HeldKindHintApproval,
		Stage:     "inline_task",
		Payload:   "pending-1",
	})
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_fail",
		Kind:      eval.HeldKindHintApproval,
		Stage:     "inline_task",
		Payload:   "pending-2",
	})
	f.AddAudit(conversation.AuditEvent{OutcomeName: "approval_pending"})

	_, err := f.Finalize(context.Background())
	if err == nil {
		t.Fatal("Finalize error = nil, want submit/drop failure")
	}
	if !errors.Is(err, submitErr) {
		t.Fatalf("Finalize error does not include submit failure: %v", err)
	}
	if !errors.Is(err, dropErr) {
		t.Fatalf("Finalize error does not include drop failure: %v", err)
	}
	if len(deps.dropped) != 1 || deps.dropped[0].ToolUseID != "toolu_committed" {
		t.Fatalf("dropped = %+v, want committed capture", deps.dropped)
	}
	if deps.dropped[0].ApprovalID != "cv-1" {
		t.Fatalf("dropped ApprovalID = %q, want cv-1", deps.dropped[0].ApprovalID)
	}
	if len(deps.audits) != 2 ||
		deps.audits[0].OutcomeName != "approval_pending" ||
		deps.audits[1].OutcomeName != "approval_hold_replay_failed" {
		t.Fatalf("audits = %+v, want eval audit then replay-failed audit", deps.audits)
	}
}

func TestFinalizerCoalescedSubmitFailureFlushesBufferedAudits(t *testing.T) {
	submitErr := errors.New("submit failed")
	deps := &finalizerTestDeps{
		submitErrs: []error{submitErr},
	}
	f := pipeline.NewFinalizer(deps)
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_hold",
		Kind:      eval.HeldKindHintApproval,
		Payload:   "pending",
	})
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_allow",
		Kind:      eval.HeldKindHintAllow,
	})
	f.AddAudit(conversation.AuditEvent{OutcomeName: "approval_pending"})
	f.AddAudit(conversation.AuditEvent{OutcomeName: "allow"})

	_, err := f.Finalize(context.Background())
	if !errors.Is(err, submitErr) {
		t.Fatalf("Finalize error = %v, want submit failure", err)
	}
	if len(deps.audits) != 2 ||
		deps.audits[0].OutcomeName != "approval_pending" ||
		deps.audits[1].OutcomeName != "allow" {
		t.Fatalf("audits = %+v, want buffered audits preserved on coalesced submit failure", deps.audits)
	}
}

func TestFinalizerReplayFailureRollsBackEveryCommittedHold(t *testing.T) {
	submitErr := errors.New("submit failed")
	dropErr := errors.New("drop failed")
	deps := &finalizerTestDeps{
		submit:     pipeline.HoldSubmitResult{ApprovalID: "cv-committed"},
		submitErrs: []error{nil, nil, submitErr},
		dropErr:    dropErr,
	}
	f := pipeline.NewFinalizer(deps)
	for _, id := range []string{"toolu_committed_1", "toolu_committed_2", "toolu_fail"} {
		f.AddCapture(pipeline.HoldCapture{
			ToolUseID: id,
			Kind:      eval.HeldKindHintApproval,
			Stage:     "inline_task",
			Payload:   "pending-" + id,
		})
	}

	_, err := f.Finalize(context.Background())
	if !errors.Is(err, submitErr) || !errors.Is(err, dropErr) {
		t.Fatalf("Finalize error = %v, want joined submit/drop failures", err)
	}
	if len(deps.dropped) != 2 {
		t.Fatalf("dropped count = %d, want 2: %+v", len(deps.dropped), deps.dropped)
	}
	if deps.dropped[0].ToolUseID != "toolu_committed_1" || deps.dropped[1].ToolUseID != "toolu_committed_2" {
		t.Fatalf("dropped = %+v, want both committed holds in order", deps.dropped)
	}
}

func TestFinalizerCoalescesApprovalWithAllowAndRewriteSiblings(t *testing.T) {
	deps := &finalizerTestDeps{
		submit: pipeline.HoldSubmitResult{ApprovalID: "cv-coalesced"},
	}
	f := pipeline.NewFinalizer(deps)
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_approval",
		Kind:      eval.HeldKindHintApproval,
		Payload:   "pending",
	})
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_allow",
		Kind:      eval.HeldKindHintAllow,
	})
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_rewrite",
		Kind:      eval.HeldKindHintRewrite,
	})

	result, err := f.Finalize(context.Background())
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !result.Coalesced || deps.submitCalls != 1 {
		t.Fatalf("result = %+v submitCalls=%d, want one coalesced submit", result, deps.submitCalls)
	}
	if len(deps.audits) != 3 {
		t.Fatalf("audit count = %d, want one coalesced audit per sibling: %+v", len(deps.audits), deps.audits)
	}
}

func TestFinalizerMissingApprovalPayloadDoesNotTriggerCoalescing(t *testing.T) {
	deps := &finalizerTestDeps{
		submit: pipeline.HoldSubmitResult{ApprovalID: "cv-should-not-submit"},
	}
	f := pipeline.NewFinalizer(deps)
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_missing_payload",
		Kind:      eval.HeldKindHintApproval,
	})
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_allow",
		Kind:      eval.HeldKindHintAllow,
	})

	result, err := f.Finalize(context.Background())
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if result.Coalesced {
		t.Fatalf("missing approval payload should not coalesce: %+v", result)
	}
	if deps.submitCalls != 0 {
		t.Fatalf("submitCalls = %d, want 0 because no capture had a payload", deps.submitCalls)
	}
}

func TestFinalizerCoalescedEvictionAuditUsesApprovalPrimary(t *testing.T) {
	deps := &finalizerTestDeps{
		submit: pipeline.HoldSubmitResult{
			ApprovalID:        "cv-new",
			EvictedApprovalID: "cv-old",
			Evicted:           errors.New("opaque evicted payload"),
		},
	}
	f := pipeline.NewFinalizer(deps)
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_allow_first",
		Kind:      eval.HeldKindHintAllow,
	})
	f.AddCapture(pipeline.HoldCapture{
		ToolUseID: "toolu_approval_second",
		Kind:      eval.HeldKindHintApproval,
		Payload:   "pending",
	})

	if _, err := f.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if len(deps.coalescedEvictedAuditToolUseID) != 1 || deps.coalescedEvictedAuditToolUseID[0] != "toolu_approval_second" {
		t.Fatalf("coalesced eviction audit primary = %v, want approval capture", deps.coalescedEvictedAuditToolUseID)
	}
}
