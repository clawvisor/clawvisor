package postproc

import (
	"context"
	"fmt"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/observability"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// applyObserveDowngrade neutralizes enforcing tool_use verdicts under
// the Observe posture (spec 02 §3). It runs AFTER the eval pass (so the
// evaluators' own audit rows are already buffered) and BEFORE commit /
// rewrite (so no hold is stored and no CLAWVISOR_BLOCKED rewrite lands).
//
// Every non-allowed verdict (a block or an approval hold) is recorded on
// the verdict metric + a tooluse.verdict span event with observed=true,
// then replaced with a plain allow so the original tool_use passes
// through byte-for-byte. Verdicts that are already Allowed (pass-through
// and mechanical resolver/credential rewrites) are left untouched.
func applyObserveDowngrade(ctx context.Context, cfg llmproxy.PostprocessConfig, verdictByTU map[string]conversation.ToolUseVerdict, toolUses []conversation.ToolUse, holdSink *capturedHoldSink) []llmproxy.ObservedToolUseVerdict {
	if !cfg.ObserveMode {
		return nil
	}
	nameByID := make(map[string]string, len(toolUses))
	for _, tu := range toolUses {
		nameByID[tu.ID] = tu.Name
	}
	seen := make(map[string]bool, len(toolUses))
	downgraded := make(map[string]bool, len(toolUses))
	var observed []llmproxy.ObservedToolUseVerdict
	for _, tu := range toolUses {
		if seen[tu.ID] {
			continue
		}
		seen[tu.ID] = true
		v, ok := verdictByTU[tu.ID]
		if !ok || v.Allowed {
			continue
		}
		decision := "block"
		if v.HeldKindHint == conversation.HeldKindHintApproval ||
			v.Outcome == conversation.OutcomeHold ||
			v.Outcome == conversation.OutcomeShortCircuit {
			decision = "hold"
		}
		observed = append(observed, llmproxy.ObservedToolUseVerdict{
			ToolUseID: tu.ID, ToolName: nameByID[tu.ID], Decision: decision, Reason: v.Reason,
		})
		observability.RecordToolUseVerdictObserved(ctx, tu.Name, decision, v.Reason, true)
		verdictByTU[tu.ID] = conversation.ToolUseVerdict{Allowed: true, Outcome: conversation.OutcomeAllow}
		downgraded[tu.ID] = true
	}
	// Drop the captured approval holds for every downgraded tool_use.
	// Rewriting verdictByTU alone is not enough: the eval pass already
	// buffered these Hold() calls in holdSink, and finalize →
	// feedFinalizer keys the finalizer's SubmitHold replay off the
	// captured Payload (not the verdict Kind). Leaving the payloads in
	// place would persist held state that Observe mode (spec 02 §3)
	// forbids. The observation record above is preserved.
	if holdSink != nil && len(downgraded) > 0 {
		dropObservedHolds(holdSink, downgraded)
	}
	return observed
}

// dropObservedHolds removes the buffered Hold() captures for the given
// downgraded tool_use IDs so the finalizer's replay skips them (nil
// Payload). The tool_use capture itself is still emitted by
// feedFinalizer for coalesce/audit rendering — only the hold submission
// is suppressed.
func dropObservedHolds(holdSink *capturedHoldSink, downgraded map[string]bool) {
	kept := holdSink.holds[:0]
	for _, h := range holdSink.holds {
		if downgraded[h.Pending.ToolUse.ID] {
			continue
		}
		kept = append(kept, h)
	}
	holdSink.holds = kept
}

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
	ctx := req.Context()

	// Eval pass: compute every verdict up-front and apply the
	// recoverable→placeholder transform. Runs BEFORE commit so commit
	// can promote transient-deny verdicts (commit calls Try on the
	// budget; on promote, it sets RecoverableReason and re-runs the
	// placeholder transform). Pre-looping also means the real rewrite
	// below just looks up the post-commit verdicts instead of running
	// transforms during emission.
	verdictByTU = make(map[string]conversation.ToolUseVerdict, len(preExtracted))
	for _, tu := range preExtracted {
		v := innerEval(tu)
		v = transformRecoverableDenyToPlaceholder(v, tu, cfg)
		verdictByTU[tu.ID] = v
	}

	if collectorErr != nil {
		// Eval pass already populated verdictByTU for whatever the
		// collector parsed before failing; the rollback below sweeps
		// any inline-task rows those evaluations created.
		return failClosed("rewriter error during tool_use extraction: " + collectorErr.Error())
	}

	// Observe posture: downgrade enforcing verdicts to observations
	// before commit so no hold is stored and no block is rewritten.
	observedVerdicts := applyObserveDowngrade(ctx, cfg, verdictByTU, preExtracted, session.holdSink)

	if commitErr := session.commitVerdictSideEffects(ctx, verdictByTU, preExtracted); commitErr != nil {
		return failClosed("verdict side-effect commit failed: " + commitErr.Error())
	}

	// Real rewrite: eval is a direct lookup against the post-commit
	// verdict map. A tool_use the collector didn't surface yields a
	// zero-value verdict (Allowed=false) — safe denial rather than
	// the bypass-commit path an on-demand fallback would create. The
	// collector and real rewrite walk the same body, so a divergence
	// here is a parser bug that should surface in tests, not get
	// silently masked.
	result, err := rewriter.Rewrite(body, contentType, func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		return verdictByTU[tu.ID]
	})
	if err != nil {
		// Fail closed: the rewriter failed mid-body so we don't know
		// whether a credentialed placeholder survived into the response.
		return failClosed("rewriter error: " + err.Error())
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
				Body:             coalescedResult.Body,
				ContentType:      contentType,
				Rewritten:        true,
				Decisions:        coalescedResult.Decisions,
				ObservedVerdicts: observedVerdicts,
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
		Body:             result.Body,
		ContentType:      contentType,
		Rewritten:        result.Rewritten,
		Decisions:        result.Decisions,
		ObservedVerdicts: observedVerdicts,
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
