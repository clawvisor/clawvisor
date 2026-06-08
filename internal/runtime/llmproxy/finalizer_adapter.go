package llmproxy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/eval"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// finalizerDeps adapts pipeline.FinalizerDeps onto the lite-proxy's
// concrete PendingApprovalCache + AuditEmitter + inline-task creator.
// Constructed per-response; the orchestrator drives all hold + audit
// commits through this surface.
type finalizerDeps struct {
	pendingApprovals PendingApprovalCache
	audit            *AuditEmitter
	agent            *store.Agent
	requestID        string
	cfg              PostprocessConfig // for CleanupEvictedInlineTask
	traceEvent       func(map[string]any)
	inlineCreator    InlineTaskCreator
}

// NewFinalizer constructs a pipeline.Finalizer wired to the supplied
// underlying PendingApprovalCache (NOT the per-response buffering
// wrapper) + cfg's audit emitter + inline-task creator. Postproc
// wraps cfg.PendingApprovals with a per-call buffering shim; the
// finalizer commits through the original cache passed here.
//
// Returns nil when pendingApprovals is nil — coalescing is a no-op
// without a cache to commit to.
func NewFinalizer(cfg PostprocessConfig, pendingApprovals PendingApprovalCache) *pipeline.Finalizer {
	if pendingApprovals == nil {
		return nil
	}
	deps := &finalizerDeps{
		pendingApprovals: pendingApprovals,
		audit:            cfg.Audit,
		agent:            AuditAgentForCfg(cfg),
		requestID:        cfg.RequestID,
		cfg:              cfg,
		inlineCreator:    cfg.InlineTaskCreator,
	}
	if cfg.Trace != nil {
		deps.traceEvent = cfg.Trace.Emit
	}
	return pipeline.NewFinalizer(deps)
}

// SubmitHold commits a buffered PendingLiteApproval to the durable
// cache.
func (d *finalizerDeps) SubmitHold(ctx context.Context, payload any) (pipeline.HoldSubmitResult, error) {
	pending, ok := payload.(PendingLiteApproval)
	if !ok {
		return pipeline.HoldSubmitResult{}, fmt.Errorf("invalid pending approval payload %T", payload)
	}
	res, err := d.pendingApprovals.Hold(ctx, pending)
	if err != nil {
		return pipeline.HoldSubmitResult{}, err
	}
	out := pipeline.HoldSubmitResult{ApprovalID: res.Pending.ID}
	if res.Evicted != nil {
		out.Evicted = res.Evicted
		out.EvictedApprovalID = res.Evicted.ID
	}
	return out, nil
}

// DropHold removes a committed hold from the durable cache (rollback
// after partial-replay failure).
func (d *finalizerDeps) DropHold(ctx context.Context, capture pipeline.HoldCapture) error {
	pending, ok := capture.Payload.(PendingLiteApproval)
	if !ok {
		return nil
	}
	if pending.ID == "" {
		// Drop uses capture.ApprovalID directly; this backfill is for
		// expirePendingInlineTask's trace path below.
		pending.ID = capture.ApprovalID
	}
	dropErr := d.pendingApprovals.Drop(ctx, ResolveRequest{
		UserID:     pending.UserID,
		AgentID:    pending.AgentID,
		Provider:   pending.Provider,
		ApprovalID: capture.ApprovalID,
	})
	expireErr := d.expirePendingInlineTask(ctx, pending, "inline_task.drop_hold_expire_failed")
	if dropErr != nil && d.traceEvent != nil {
		d.traceEvent(map[string]any{
			"event":       "inline_task.drop_hold_failed",
			"request_id":  d.requestID,
			"user_id":     pending.UserID,
			"agent_id":    pending.AgentID,
			"approval_id": capture.ApprovalID,
			"err":         dropErr.Error(),
		})
	}
	return errors.Join(dropErr, expireErr)
}

// BuildCoalescedHold constructs the single PendingLiteApproval that
// supersedes every buffered capture. The first approval-class capture
// becomes the primary; the rest land in Additional. Allow / Rewrite
// siblings (no Payload) contribute their ToolUse to the rendered prompt
// without adding a real hold record.
func (d *finalizerDeps) BuildCoalescedHold(captures []pipeline.HoldCapture) pipeline.CoalescedHold {
	primaryIdx := -1
	for i, c := range captures {
		if c.Kind == eval.HeldKindHintApproval && c.Payload != nil {
			primaryIdx = i
			break
		}
	}
	if primaryIdx < 0 {
		// No approval-class capture has a Payload — fall back to the
		// first capture with a Payload, then to index 0.
		for i, c := range captures {
			if c.Payload != nil {
				primaryIdx = i
				break
			}
		}
		if primaryIdx < 0 {
			primaryIdx = 0
		}
	}
	primary, ok := captures[primaryIdx].Payload.(PendingLiteApproval)
	if !ok {
		// This should be unreachable because pipeline.shouldCoalesce
		// requires an approval capture with a non-nil payload, and
		// SubmitHold will fail closed if a bad payload reaches it.
		return pipeline.CoalescedHold{Payload: nil}
	}
	pending := PendingLiteApproval{
		ToolUse:      primary.ToolUse,
		Inspector:    primary.Inspector,
		Fingerprint:  primary.Fingerprint,
		Reason:       primary.Reason,
		PrimaryIndex: primaryIdx,
	}
	pending.Additional = make([]HeldToolUse, 0, len(captures)-1)
	for i, c := range captures {
		if i == primaryIdx {
			continue
		}
		entry := HeldToolUse{Kind: heldKindForCapture(c)}
		if p, ok := c.Payload.(PendingLiteApproval); ok {
			entry.ToolUse = p.ToolUse
			entry.Inspector = p.Inspector
			entry.Fingerprint = p.Fingerprint
			entry.Reason = p.Reason
		} else {
			entry.ToolUse = c.ToolUse
			entry.Inspector = InspectorVerdictFromSnapshot(c.InspectorSnapshot)
		}
		pending.Additional = append(pending.Additional, entry)
	}
	// Carry forward the user/agent/provider/conversation identity from
	// the primary so the cache submits with the right keys.
	pending.UserID = primary.UserID
	pending.AgentID = primary.AgentID
	pending.Provider = primary.Provider
	pending.ConversationID = primary.ConversationID

	return pipeline.CoalescedHold{
		Payload: pending,
		EvictedAuditFor: func(primaryCap pipeline.HoldCapture, evictedID string) conversation.AuditEvent {
			ev := conversation.AuditEvent{
				Decision:    conversation.DecisionBlock,
				OutcomeName: "approval_evicted",
				Reason:      "superseded pending approval " + evictedID,
			}
			if p, ok := primaryCap.Payload.(PendingLiteApproval); ok {
				ev.ToolUse = p.ToolUse
				ev.InspectorVerdict = InspectorSnapshot(p.Inspector)
			} else {
				ev.ToolUse = primaryCap.ToolUse
				ev.InspectorVerdict = primaryCap.InspectorSnapshot
			}
			return ev
		},
		PerToolAuditFor: func(c pipeline.HoldCapture, approvalID string) conversation.AuditEvent {
			kindLabel := string(heldKindForCapture(c))
			reason := "held under coalesced approval " + approvalID + " (originally classified as " + kindLabel + ")"
			ev := conversation.AuditEvent{
				Decision:    conversation.DecisionBlock,
				OutcomeName: "coalesced_approval_pending",
				Reason:      reason,
			}
			if p, ok := c.Payload.(PendingLiteApproval); ok {
				ev.ToolUse = p.ToolUse
				ev.InspectorVerdict = InspectorSnapshot(p.Inspector)
			} else {
				ev.ToolUse = c.ToolUse
				ev.InspectorVerdict = c.InspectorSnapshot
			}
			return ev
		},
		Prompt: func(approvalID string) string {
			return coalescedApprovalPrompt(pending.AllHolds(), approvalID)
		},
	}
}

// BuildReplayFailedAudit formats the audit row when a single replay
// submit fails.
func (d *finalizerDeps) BuildReplayFailedAudit(c pipeline.HoldCapture, err error) conversation.AuditEvent {
	p, _ := c.Payload.(PendingLiteApproval)
	return conversation.AuditEvent{
		ToolUse:          p.ToolUse,
		InspectorVerdict: InspectorSnapshot(p.Inspector),
		Decision:         conversation.DecisionBlock,
		OutcomeName:      "approval_hold_replay_failed",
		Reason:           err.Error(),
	}
}

// BuildEvictedAudit formats the audit row when a single submit
// evicts a prior hold.
func (d *finalizerDeps) BuildEvictedAudit(c pipeline.HoldCapture, approvalID string) conversation.AuditEvent {
	p, _ := c.Payload.(PendingLiteApproval)
	return conversation.AuditEvent{
		ToolUse:          p.ToolUse,
		InspectorVerdict: InspectorSnapshot(p.Inspector),
		Decision:         conversation.DecisionBlock,
		OutcomeName:      "approval_evicted",
		Reason:           "superseded pending approval " + approvalID,
	}
}

// CleanupEvictedHold runs the lite-proxy's inline-task cleanup for
// an evicted hold (expiry + audit row, depending on Stage).
func (d *finalizerDeps) CleanupEvictedHold(ctx context.Context, evicted any) {
	pending, ok := evicted.(*PendingLiteApproval)
	if !ok {
		return
	}
	CleanupEvictedInlineTask(ctx, d.cfg, pending)
}

// RollbackPendingTask expires any pending inline-task row a buffered
// hold created when the turn fails before commit.
func (d *finalizerDeps) RollbackPendingTask(ctx context.Context, c pipeline.HoldCapture) {
	pending, ok := c.Payload.(PendingLiteApproval)
	if !ok {
		return
	}
	_ = d.expirePendingInlineTask(ctx, pending, "inline_task.rollback_expire_failed")
}

func (d *finalizerDeps) expirePendingInlineTask(ctx context.Context, pending PendingLiteApproval, eventName string) error {
	pendingCreator, ok := d.inlineCreator.(InlineTaskPendingCreator)
	if !ok || pendingCreator == nil {
		return nil
	}
	if pending.PendingTaskID == "" || pending.UserID == "" {
		return nil
	}
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	err := pendingCreator.ExpireInlineTask(rollbackCtx, pending.PendingTaskID, pending.UserID)
	cancel()
	if err != nil && d.traceEvent != nil {
		d.traceEvent(map[string]any{
			"event":       eventName,
			"request_id":  d.requestID,
			"user_id":     pending.UserID,
			"agent_id":    pending.AgentID,
			"approval_id": pending.ID,
			"task_id":     pending.PendingTaskID,
			"err":         err.Error(),
		})
	}
	return err
}

// WriteAudit emits a typed AuditEvent to the durable audit store.
func (d *finalizerDeps) WriteAudit(ctx context.Context, ev conversation.AuditEvent) {
	if d.audit == nil || d.agent == nil {
		return
	}
	d.audit.WriteAuditEvent(ctx, d.agent, d.requestID, ev)
}

// heldKindForCapture maps the orchestrator's eval.HeldKindHint into
// the llmproxy.HeldToolUseKind the per-tool audit row + coalesced
// prompt rendering still expect.
func heldKindForCapture(c pipeline.HoldCapture) HeldToolUseKind {
	switch c.Kind {
	case eval.HeldKindHintApproval:
		return HeldKindApproval
	case eval.HeldKindHintAllow:
		return HeldKindAllow
	case eval.HeldKindHintRewrite:
		return HeldKindRewrite
	case eval.HeldKindHintDeny:
		return HeldKindDeny
	}
	return HeldKindDeny
}

// coalescedApprovalPrompt renders the human-facing prompt for a
// coalesced hold. Mirrors the legacy postproc.coalescedApprovalPrompt
// formatter; moved here so postproc shrinks to a thin coordinator.
func coalescedApprovalPrompt(uses []HeldToolUse, approvalID string) string {
	var b strings.Builder
	b.WriteString("Clawvisor paused this turn for approval (")
	b.WriteString(strconv.Itoa(len(uses)))
	b.WriteString(" tool calls).")
	for i, held := range uses {
		b.WriteString("\n\n")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		if name := strings.TrimSpace(held.ToolUse.Name); name != "" {
			b.WriteString("`")
			b.WriteString(name)
			b.WriteString("`")
		} else {
			b.WriteString("(unnamed tool)")
		}
		switch held.Kind {
		case HeldKindApproval:
			if reason := strings.TrimSpace(held.Reason); reason != "" {
				b.WriteString(" — approval required: ")
				b.WriteString(reason)
			} else {
				b.WriteString(" — approval required")
			}
		case HeldKindAllow:
			b.WriteString(" — held alongside (would auto-allow on its own)")
		case HeldKindRewrite:
			b.WriteString(" — held alongside (would auto-allow with credential rewrite on its own)")
		}
		if preview := conversation.MakeToolInputPreview(held.ToolUse.Input); preview != "" {
			b.WriteString("\n   Input: ")
			b.WriteString(preview)
		}
	}
	b.WriteString("\n\nReply `yes` or `y` to approve all calls and run them in order, `no` or `n` to deny the whole turn, or `task` to scope this work under a Clawvisor task that covers every call above.")
	b.WriteString(ApprovalIDFooter(approvalID))
	return b.String()
}
