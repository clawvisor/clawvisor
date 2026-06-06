package postproc

import (
	"context"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// Postprocess inspects, rewrites, and audits the upstream response.
// The pipeline factory (registered via pipelineeval) drives per-tool
// evaluation; the pipeline.Finalizer owns the response-level
// coalesce / replay / audit-flush decisions. This function shrinks
// to coordination: extract tool_uses, run eval, run rewriter, hand
// off to Finalize, optionally re-run the rewriter with the
// coalesced prompt.
//
// Phase D leak cleanup: finalization no longer lives in postproc.
func Postprocess(req *http.Request, body []byte, contentType string, cfg llmproxy.PostprocessConfig) llmproxy.PostprocessResult {
	if cfg.Inspector == nil {
		return llmproxy.PostprocessResult{Body: body, ContentType: contentType, SkippedReason: "no inspector configured"}
	}

	registry := cfg.ResponseRegistry
	if registry == nil {
		registry = conversation.DefaultResponseRegistry()
	}

	// MatchesResponse on the existing rewriters checks the request's host;
	// for the lite-proxy endpoint the host is `clawvisor.example`, not
	// `api.anthropic.com`. Use the parser registry instead — it's
	// route-keyed via ParserForRoute (added for lite-proxy).
	rewriter := matchByRoute(req, registry)
	if rewriter == nil {
		return llmproxy.PostprocessResult{Body: body, ContentType: contentType, SkippedReason: "no rewriter for route"}
	}

	session := newPostprocessSession(cfg)

	var preExtracted []conversation.ToolUse
	failClosed := func(reason string) llmproxy.PostprocessResult {
		session.rollback(req.Context(), preExtracted)
		return llmproxy.PostprocessResult{
			Body:          nil,
			ContentType:   contentType,
			SkippedReason: reason,
		}
	}

	// Pre-extract tool_uses so the factory can run pipeline.EvaluateToolUses
	// ONCE on the full sibling set. The collector pass discards the
	// rewritten body; the real rewrite happens in the second pass with
	// the pre-computed verdicts.
	collectorEval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		preExtracted = append(preExtracted, tu)
		return conversation.ToolUseVerdict{Allowed: true}
	}
	if _, err := rewriter.Rewrite(body, contentType, collectorEval); err != nil && len(preExtracted) == 0 {
		return failClosed("rewriter error during tool_use extraction: " + err.Error())
	}

	innerEval := session.evaluator(req, rewriter.Name(), preExtracted)

	// Capture per-tool verdicts so the finalizer can classify them.
	verdictByTU := make(map[string]conversation.ToolUseVerdict, len(preExtracted))
	eval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		v := innerEval(tu)
		verdictByTU[tu.ID] = v
		return v
	}

	result, err := rewriter.Rewrite(body, contentType, eval)
	if err != nil {
		// Fail closed: the rewriter failed mid-body so we don't know
		// whether a credentialed placeholder survived into the response.
		return failClosed("rewriter error: " + err.Error())
	}

	ctx := req.Context()
	finalResult, finalErr := session.finalize(ctx, preExtracted, verdictByTU)
	if finalErr != nil {
		return failClosed("approval hold storage failed: " + finalErr.Error())
	}

	if finalResult.Coalesced {
		// Re-run the rewriter with a coalesced eval substituting the
		// human-facing prompt at the primary tool_use's slot.
		firstReplaced := false
		coalescedEval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
			out := conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: approval required (coalesced turn)",
			}
			if !firstReplaced {
				out.SubstituteWith = finalResult.CoalescedPrompt
				firstReplaced = true
			}
			return out
		}
		coalescedResult, coalescedErr := rewriter.Rewrite(body, contentType, coalescedEval)
		if coalescedErr == nil {
			return llmproxy.PostprocessResult{
				Body:        coalescedResult.Body,
				ContentType: contentType,
				Rewritten:   true,
				Decisions:   coalescedResult.Decisions,
			}
		}
		// Coalesced re-run failed; fall through to first-pass result.
		return llmproxy.PostprocessResult{
			Body:        result.Body,
			ContentType: contentType,
			Rewritten:   result.Rewritten,
			Decisions:   result.Decisions,
		}
	}

	return llmproxy.PostprocessResult{
		Body:        result.Body,
		ContentType: contentType,
		Rewritten:   result.Rewritten,
		Decisions:   result.Decisions,
	}
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
	holdByTU := make(map[string]capturedHold, len(holdSink.holds))
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

func flushDirect(ctx context.Context, cfg llmproxy.PostprocessConfig, auditBuf *pendingAuditEventBuffer) {
	if cfg.Audit == nil || auditBuf == nil {
		return
	}
	agent := llmproxy.AuditAgentForCfg(cfg)
	if agent == nil {
		return
	}
	for _, ev := range auditBuf.entries {
		cfg.Audit.WriteAuditEvent(ctx, agent, cfg.RequestID, ev)
	}
}

// selectToolUseEvaluator dispatches to the cfg-supplied
// ToolUseEvaluatorFactory. Nil is a programmer error — the handler
// (and every test that exercises Postprocess) must assign
// pipelineeval.Factory to cfg.ToolUseEvaluatorFactory explicitly.
//
// toolUses is the pre-extracted sibling set when known. The returned
// evaluator appends audit rows through emit for the owning session.
func selectToolUseEvaluator(req *http.Request, cfg llmproxy.PostprocessConfig, provider conversation.Provider, toolUses []conversation.ToolUse, emit func(conversation.AuditEvent)) conversation.ToolUseEvaluator {
	if cfg.ToolUseEvaluatorFactory == nil {
		panic("llmproxy/postproc: PostprocessConfig.ToolUseEvaluatorFactory is required — assign pipelineeval.Factory")
	}
	return cfg.ToolUseEvaluatorFactory(req, cfg, provider, toolUses, emit)
}

// matchByRoute returns the response rewriter the registry has indexed
// for the inbound request's URL path. Returns nil when no parser
// matches; the caller short-circuits with SkippedReason.
func matchByRoute(req *http.Request, registry *conversation.ResponseRegistry) conversation.ResponseRewriter {
	if registry == nil || req == nil || req.URL == nil {
		return nil
	}
	parsers := conversation.DefaultRegistry()
	parser := parsers.ParserForRoute(req.URL.Path)
	if parser == nil {
		return nil
	}
	return registry.ForProvider(parser.Name())
}

// matchByRouteStreaming is the streaming counterpart to matchByRoute.
func matchByRouteStreaming(req *http.Request, registry *conversation.ResponseRegistry) conversation.StreamingResponseRewriter {
	if registry == nil || req == nil || req.URL == nil {
		return nil
	}
	parsers := conversation.DefaultRegistry()
	parser := parsers.ParserForRoute(req.URL.Path)
	if parser == nil {
		return nil
	}
	return registry.ForProviderStreaming(parser.Name())
}
