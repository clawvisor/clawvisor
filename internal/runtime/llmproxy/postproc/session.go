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
// during evaluation (scope-drift mints, recoverable-deny migrations).
// rollback() iterates them so a request whose response is later
// failClosed'd doesn't leak orphan entries. trackSubstitution is the
// single entry point; postproc.go and stream.go call it after each
// eval verdict commits.
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

// trackSubstitution records a pending-substitution registry write so
// rollback() can revert it if the response is later failClosed'd. Nil
// keys are ignored so callers can pass through transform results
// directly without branching.
func (s *postprocessSession) trackSubstitution(key *llmproxy.PendingSubstitutionKey) {
	if s == nil || key == nil {
		return
	}
	s.substitutions = append(s.substitutions, *key)
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
