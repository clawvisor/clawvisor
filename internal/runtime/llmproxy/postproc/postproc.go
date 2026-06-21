package postproc

import (
	"context"
	"fmt"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// Postprocess inspects, rewrites, and audits the upstream response.
// The pipeline factory (registered via pipelineeval) drives per-tool
// evaluation; the pipeline.Finalizer owns the response-level
// coalesce / replay / audit-flush decisions. This function shrinks
// to coordination: extract tool_uses, run eval, run rewriter, hand
// off to Finalize, optionally re-run the rewriter with the
// coalesced prompt.
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
	var verdictByTU map[string]conversation.ToolUseVerdict
	failClosed := func(reason string) llmproxy.PostprocessResult {
		session.rollback(req.Context(), preExtracted, verdictByTU)
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
	var collectorErr error
	if _, err := rewriter.Rewrite(body, contentType, collectorEval); err != nil {
		collectorErr = err
	}

	innerEval := session.evaluator(req, rewriter.Name(), preExtracted)

	// Capture per-tool verdicts so the finalizer can classify them.
	verdictByTU = make(map[string]conversation.ToolUseVerdict, len(preExtracted))
	eval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		v := innerEval(tu)
		v = transformTransientDenyToRecoverable(req.Context(), v, tu, cfg, session.trackTransientConsumed)
		v = transformRecoverableDenyToPlaceholder(v, tu, cfg)
		verdictByTU[tu.ID] = v
		return v
	}

	if collectorErr != nil {
		// The collector may have parsed tool_uses before failing later
		// in the body. Evaluate only those parsed tools so any pending
		// inline-task rows they create are rolled back below; the audit
		// rows describe this local policy/rollback work, not an upstream
		// tool call that reached the harness.
		for _, tu := range preExtracted {
			eval(tu)
		}
		return failClosed("rewriter error during tool_use extraction: " + collectorErr.Error())
	}

	result, err := rewriter.Rewrite(body, contentType, eval)
	if err != nil {
		// Fail closed: the rewriter failed mid-body so we don't know
		// whether a credentialed placeholder survived into the response.
		return failClosed("rewriter error: " + err.Error())
	}

	ctx := req.Context()
	if commitErr := session.commitVerdictSideEffects(ctx, verdictByTU, preExtracted); commitErr != nil {
		return failClosed("verdict side-effect commit failed: " + commitErr.Error())
	}
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
				Allowed:                false,
				Reason:                 "Clawvisor: approval required (coalesced turn)",
				SuppressSubstituteText: true,
			}
			if !firstReplaced {
				out.SubstituteWith = finalResult.CoalescedPrompt
				out.SuppressSubstituteText = false
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
		dropErr := session.dropCommittedAndRollback(ctx, finalResult.CoalescedCapture)
		reason := "coalesced approval rewrite failed: " + coalescedErr.Error()
		if dropErr != nil {
			reason += "; rollback failed: " + dropErr.Error()
		}
		return failClosed(reason)
	}

	return llmproxy.PostprocessResult{
		Body:        result.Body,
		ContentType: contentType,
		Rewritten:   result.Rewritten,
		Decisions:   result.Decisions,
	}
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
// ToolUseEvaluatorFactory. Missing factories fail closed instead of
// panicking the serving goroutine.
//
// toolUses is the pre-extracted sibling set when known. The returned
// evaluator appends audit rows through emit for the owning session.
func selectToolUseEvaluator(req *http.Request, cfg llmproxy.PostprocessConfig, provider conversation.Provider, toolUses []conversation.ToolUse, emit func(conversation.AuditEvent)) conversation.ToolUseEvaluator {
	if cfg.ToolUseEvaluatorFactory == nil {
		reason := fmt.Sprintf("Clawvisor: postprocess evaluator is not configured for provider %q", provider)
		return func(conversation.ToolUse) conversation.ToolUseVerdict {
			return conversation.ToolUseVerdict{
				Allowed: false,
				Outcome: conversation.OutcomeDeny,
				Reason:  reason,
			}
		}
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
