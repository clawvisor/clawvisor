package postproc

import (
	"context"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// postprocessSession owns the per-response adapters that bridge
// evaluator side effects into pipeline.Finalizer. Buffered and
// streaming postprocess both use this shape so capture/finalize
// lifecycle details stay in one place.
type postprocessSession struct {
	baseCfg                  llmproxy.PostprocessConfig
	evalCfg                  llmproxy.PostprocessConfig
	originalPendingApprovals llmproxy.PendingApprovalCache
	holdSink                 *capturedHoldSink
	auditBuf                 *pendingAuditEventBuffer
	finalizer                *pipeline.Finalizer
	fed                      bool
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
	if s == nil || s.finalizer == nil {
		return
	}
	s.feed(toolUses, verdictByTU)
	s.finalizer.Rollback(ctx)
}

func (s *postprocessSession) captures() []pipeline.HoldCapture {
	if s == nil || s.finalizer == nil {
		return nil
	}
	return s.finalizer.Captures()
}
