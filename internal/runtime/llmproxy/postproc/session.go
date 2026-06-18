package postproc

import (
	"context"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// postprocessSession owns the per-response adapters that bridge
// evaluator side effects into pipeline.Finalizer. Buffered and
// streaming postprocess both use this shape so capture/finalize
// lifecycle details stay in one place.
//
// substitutions tracks pending-substitution registry writes that fired
// during evaluation (scope-drift mints, recoverable-deny migrations,
// inline-task auto-approve). rollback() iterates them so a request
// whose response is later failClosed'd doesn't leak orphan entries.
// commitSubstitutions is the single entry point — it walks every
// verdict's PendingSubstitution spec and registers it, recording the
// key for rollback. Evaluators MUST NOT call into the registry
// themselves; the spec-on-verdict pattern keeps the verdict pure data
// and concentrates rollback in one place.
type postprocessSession struct {
	baseCfg                  llmproxy.PostprocessConfig
	evalCfg                  llmproxy.PostprocessConfig
	originalPendingApprovals llmproxy.PendingApprovalCache
	holdSink                 *capturedHoldSink
	auditBuf                 *pendingAuditEventBuffer
	finalizer                *pipeline.Finalizer
	fed                      bool
	substitutions            []llmproxy.PendingSubstitutionKey
}

func newPostprocessSession(cfg llmproxy.PostprocessConfig) *postprocessSession {
	holdSink := &capturedHoldSink{}
	evalCfg := cfg
	originalPendingApprovals := cfg.PendingApprovals
	if originalPendingApprovals != nil {
		evalCfg.PendingApprovals = newHoldCapturingApprovalCache(originalPendingApprovals, holdSink)
	}
	return &postprocessSession{
		baseCfg:                  cfg,
		evalCfg:                  evalCfg,
		originalPendingApprovals: originalPendingApprovals,
		holdSink:                 holdSink,
		auditBuf:                 &pendingAuditEventBuffer{},
		finalizer:                llmproxy.NewFinalizer(cfg, originalPendingApprovals),
	}
}

func (s *postprocessSession) evaluator(req *http.Request, provider conversation.Provider, toolUses []conversation.ToolUse) conversation.ToolUseEvaluator {
	if s == nil {
		return func(conversation.ToolUse) conversation.ToolUseVerdict {
			return conversation.ToolUseVerdict{Allowed: true}
		}
	}
	return selectToolUseEvaluator(req, s.evalCfg, provider, toolUses, s.emitAudit)
}

func (s *postprocessSession) emitAudit(ev conversation.AuditEvent) {
	if s == nil || s.auditBuf == nil {
		return
	}
	s.auditBuf.entries = append(s.auditBuf.entries, ev)
}

func (s *postprocessSession) feed(toolUses []conversation.ToolUse, verdictByTU map[string]conversation.ToolUseVerdict) {
	if s == nil || s.fed {
		return
	}
	s.fed = true
	feedFinalizer(s.finalizer, toolUses, s.holdSink, s.auditBuf, verdictByTU)
}

func (s *postprocessSession) finalize(ctx context.Context, toolUses []conversation.ToolUse, verdictByTU map[string]conversation.ToolUseVerdict) (pipeline.FinalizeResult, error) {
	if s == nil {
		return pipeline.FinalizeResult{}, nil
	}
	s.feed(toolUses, verdictByTU)
	if s.finalizer != nil && s.originalPendingApprovals != nil {
		return s.finalizer.Finalize(ctx)
	}
	flushDirect(ctx, s.baseCfg, s.auditBuf)
	return pipeline.FinalizeResult{}, nil
}

func (s *postprocessSession) rollback(ctx context.Context, toolUses []conversation.ToolUse, verdictByTU map[string]conversation.ToolUseVerdict) {
	if s == nil {
		return
	}
	if s.finalizer != nil {
		s.feed(toolUses, verdictByTU)
		s.finalizer.Rollback(ctx)
	}
	s.rollbackSubstitutions(ctx)
}

// commitSubstitutions registers every verdict.PendingSubstitution
// spec against the registry, walking decisions in turn order. Called
// AFTER all evaluators have produced verdicts so the verdict itself
// stays free of registry side-effects.
//
// On registry write failure: any TaskRollback the spec carries (today
// only the inline-task auto-approve path populates this) is honoured
// via the configured InlineApprovedTaskExpirer — the orphan task is
// expired with a detached context so a mid-request client disconnect
// doesn't cancel the rollback. Already-registered substitutions
// recorded earlier in the walk are rolled back via the standard
// session.rollback path so the registry doesn't end up partially
// populated for this request. The function returns the failing error
// so the caller (postproc / stream) can fail-closed the response.
//
// Recorded keys feed rollback() — if a later step in the postprocess
// pipeline fails, every registry write made on behalf of this request
// is undone in one place.
func (s *postprocessSession) commitSubstitutions(ctx context.Context, verdictByTU map[string]conversation.ToolUseVerdict, toolUses []conversation.ToolUse) error {
	if s == nil {
		return nil
	}
	registry := s.baseCfg.AuthorizationContext.ScopeDrifts
	if registry == nil {
		return nil
	}
	agentID := s.baseCfg.AgentContext.AgentID
	conversationID := s.baseCfg.AuditContext.ConversationID
	// Walk in tool_use order (not map order) so audit and tests see a
	// deterministic registration sequence.
	for _, tu := range toolUses {
		v, ok := verdictByTU[tu.ID]
		if !ok || v.PendingSubstitution == nil {
			continue
		}
		spec := v.PendingSubstitution
		if agentID == "" || conversationID == "" {
			// Identity tuple incomplete — the same guard the evaluators
			// applied before populating the spec. Skip rather than mint
			// a key that would collide across concurrent conversations.
			continue
		}
		key := llmproxy.PendingSubstitutionKey{
			AgentID:        agentID,
			ConversationID: conversationID,
			ToolUseID:      tu.ID,
		}
		err := registry.RegisterPendingSubstitution(ctx, key, llmproxy.PendingSubstitution{
			DriftID:           spec.DriftID,
			MenuText:          spec.MenuText,
			OriginalToolName:  spec.OriginalToolName,
			OriginalToolInput: append([]byte(nil), spec.OriginalToolInput...),
		})
		if err != nil {
			// Roll back the task this spec was guarding, if any. The
			// expirer interface is opt-in; legacy creator stubs that
			// don't implement it strand the orphan, audit-traced for
			// operators below.
			if spec.TaskRollback != nil {
				s.expireRollbackTask(ctx, spec.TaskRollback, err)
			}
			return err
		}
		s.substitutions = append(s.substitutions, key)
	}
	return nil
}

// expireRollbackTask invokes the configured InlineApprovedTaskExpirer
// to unwind an orphan task left behind when registration failed.
// Detached context with a short timeout protects the rollback from a
// canceled client connection (the same condition that may have caused
// the registry write to fail in the first place).
func (s *postprocessSession) expireRollbackTask(ctx context.Context, handle *conversation.PendingSubstitutionTaskRollback, regErr error) {
	creator := s.baseCfg.InlineTaskCreator
	if creator == nil || handle == nil {
		return
	}
	trace := llmproxy.TraceLoggerEmit(s.baseCfg.AuditContext.Trace)
	expirer, ok := creator.(llmproxy.InlineApprovedTaskExpirer)
	if !ok {
		// The creator implementation predates the rollback interface;
		// can't undo. Trace so operators can see why an orphan exists.
		if trace != nil {
			trace("inline_task.auto_approve_rollback_unavailable",
				"task_id", handle.TaskID,
				"reason", "InlineTaskCreator does not implement InlineApprovedTaskExpirer",
			)
		}
		return
	}
	rollbackCtx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := expirer.ExpireInlineApprovedTask(rollbackCtx, handle.TaskID, handle.UserID); err != nil {
		if trace != nil {
			trace("inline_task.auto_approve_rollback_failed",
				"task_id", handle.TaskID,
				"err", err.Error(),
				"register_err", regErr.Error(),
			)
		}
	}
}

// rollbackSubstitutions deletes every tracked registry write. Idempotent
// — subsequent calls are no-ops because the slice is cleared.
func (s *postprocessSession) rollbackSubstitutions(ctx context.Context) {
	if s == nil || len(s.substitutions) == 0 {
		return
	}
	registry := s.baseCfg.AuthorizationContext.ScopeDrifts
	if registry == nil {
		s.substitutions = nil
		return
	}
	for _, key := range s.substitutions {
		registry.DeletePendingSubstitution(ctx, key)
	}
	s.substitutions = nil
}

func (s *postprocessSession) dropCommitted(ctx context.Context, capture *pipeline.HoldCapture) error {
	if s == nil || s.finalizer == nil || capture == nil {
		return nil
	}
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	return s.finalizer.DropCommittedHold(cleanupCtx, *capture)
}

func (s *postprocessSession) dropCommittedAndRollback(ctx context.Context, capture *pipeline.HoldCapture) error {
	if s == nil || s.finalizer == nil || capture == nil {
		return nil
	}
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	return s.finalizer.DropCommittedAndRollback(cleanupCtx, *capture)
}

func (s *postprocessSession) dropAllCommittedAndRollback(ctx context.Context) error {
	if s == nil || s.finalizer == nil {
		return nil
	}
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	return s.finalizer.DropAllCommittedAndRollback(cleanupCtx)
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func (s *postprocessSession) captures() []pipeline.HoldCapture {
	if s == nil || s.finalizer == nil {
		return nil
	}
	return s.finalizer.Captures()
}

// feedFinalizer transfers per-tool eval outcomes + audit events into
// the finalizer. Captures every tool_use (whether or not it called
// Hold) so the coalesce decision sees Allow/Rewrite siblings
// alongside the held Approvals. Captures that didn't Hold carry a
// nil Payload; replay skips them.
//
// orderedToolUses preserves the response order of tool_uses so the
// coalesced primary is selected deterministically + each capture
// carries its ToolUse for audit/prompt rendering.
func feedFinalizer(
	finalizer *pipeline.Finalizer,
	orderedToolUses []conversation.ToolUse,
	holdSink *capturedHoldSink,
	auditBuf *pendingAuditEventBuffer,
	verdictByTU map[string]conversation.ToolUseVerdict,
) {
	if finalizer == nil {
		return
	}
	holdCount := 0
	if holdSink != nil {
		holdCount = len(holdSink.holds)
	}
	holdByTU := make(map[string]capturedHold, holdCount)
	if holdSink != nil {
		for _, h := range holdSink.holds {
			holdByTU[h.Pending.ToolUse.ID] = h
		}
	}
	// Inspector verdicts surface through the buffered audit events
	// the factory emitted. Allow / Rewrite siblings (no Hold) carry
	// their inspector projection here so the coalesced renderer can
	// fold them into the prompt with full audit detail.
	auditByTU := make(map[string]conversation.AuditEvent)
	if auditBuf != nil {
		for _, ev := range auditBuf.entries {
			auditByTU[ev.ToolUse.ID] = ev
		}
	}
	for _, tu := range orderedToolUses {
		kind := holdKindFromVerdict(verdictByTU, tu.ID)
		c := pipeline.HoldCapture{
			ToolUse:   tu,
			ToolUseID: tu.ID,
			Kind:      kind,
		}
		if h, ok := holdByTU[tu.ID]; ok {
			c.ApprovalID = h.Pending.ID
			c.Stage = string(h.Pending.Stage)
			c.Payload = h.Pending
			c.InspectorSnapshot = llmproxy.InspectorSnapshot(h.Pending.Inspector)
		} else if ev, ok := auditByTU[tu.ID]; ok {
			c.InspectorSnapshot = ev.InspectorVerdict
		}
		finalizer.AddCapture(c)
	}
	if auditBuf != nil {
		for _, ev := range auditBuf.entries {
			finalizer.AddAudit(ev)
		}
	}
}

func holdKindFromVerdict(
	verdictByTU map[string]conversation.ToolUseVerdict,
	tuID string,
) conversation.HeldKindHint {
	if v, ok := verdictByTU[tuID]; ok {
		return pipeline.ClassifyVerdict(v)
	}
	return conversation.HeldKindHintDeny
}
