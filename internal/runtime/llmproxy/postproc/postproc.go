package postproc

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// Postprocess inspects, rewrites, and audits the upstream response.
// The pipeline factory (registered via pipelineeval) drives per-tool
// evaluation; this function wraps that with buffered captures, the
// coalesce decision over the whole turn, and the legacy replay
// fallback when coalescence isn't applicable or fails.
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

	auditAgent := llmproxy.AuditAgentForCfg(cfg)

	// Coalescence capture state. Pass 1 runs with a buffering wrapper
	// over both PendingApprovals and the audit emission so we can:
	//   * detect when multiple tool_uses in one turn need approval
	//   * detect the inline-task path (Stage != StageTool) to skip
	//     coalescence for it
	//   * decide a final shape (legacy: replay buffers; coalesce:
	//     discard buffers and write one coalesced hold + per-tool
	//     coalesced-pending audit rows)
	originalPendingApprovals := cfg.PendingApprovals
	holdSink := &capturedHoldSink{}
	if originalPendingApprovals != nil {
		cfg.PendingApprovals = newHoldCapturingApprovalCache(originalPendingApprovals, holdSink)
	}
	pendingAuditEvents := &pendingAuditEventBuffer{}
	var captures []evalCapture
	failClosed := func(reason string) llmproxy.PostprocessResult {
		rollbackBufferedPendingTasks(req.Context(), cfg, holdSink)
		return llmproxy.PostprocessResult{
			Body:          nil,
			ContentType:   contentType,
			SkippedReason: reason,
		}
	}

	// Pre-extract tool_uses so the factory can run pipeline.EvaluateToolUses
	// ONCE on the full sibling set (response-level orchestration). The
	// collector pass discards the rewritten body (Allowed=true with no
	// mutations); the real rewrite happens in the second pass below
	// with the pre-computed verdicts.
	//
	// Collector errors are tolerated when at least one tool_use was
	// collected — the real rewriter pass below will surface the error
	// and trigger failClosed. This preserves legacy "rewriter errors
	// AFTER calling eval still create side effects" semantics, which a
	// few tests pin via evalThenErrorRewriter.
	var preExtracted []conversation.ToolUse
	collectorEval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		preExtracted = append(preExtracted, tu)
		return conversation.ToolUseVerdict{Allowed: true}
	}
	if _, err := rewriter.Rewrite(body, contentType, collectorEval); err != nil && len(preExtracted) == 0 {
		return failClosed("rewriter error during tool_use extraction: " + err.Error())
	}

	innerEval := selectToolUseEvaluator(req, cfg, rewriter.Name(), preExtracted, pendingAuditEvents)

	// Outer eval wraps innerEval and records the kind + decision
	// context for the coalesce post-pass. Two side channels feed the
	// capture: holdSink.holds (set by the buffered PendingApprovals
	// wrapper) and pendingAuditEvents.entries (set by the buffered audit
	// closure). In response-level mode (buffered path) all pipeline
	// side effects fire upfront during selectToolUseEvaluator, so the
	// capture matches entries by tool_use ID instead of by sequence.
	eval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		v := innerEval(tu)
		c := evalCapture{Use: tu, Kind: classifyVerdict(v)}
		if holdSink != nil {
			for i := len(holdSink.holds) - 1; i >= 0; i-- {
				h := holdSink.holds[i]
				if h.Pending.ToolUse.ID == tu.ID {
					c.HoldID = h.Pending.ID
					c.Stage = h.Pending.Stage
					c.Inspector = h.Pending.Inspector
					c.Fingerprint = h.Pending.Fingerprint
					c.Reason = h.Pending.Reason
					break
				}
			}
		}
		if pendingAuditEvents != nil {
			for i := len(pendingAuditEvents.entries) - 1; i >= 0; i-- {
				entry := pendingAuditEvents.entries[i]
				if entry.ToolUse.ID == tu.ID {
					if c.Inspector.Source == "" {
						c.Inspector = entry.InspectorVerdict
					}
					if c.Reason == "" {
						c.Reason = entry.Reason
					}
					if c.TaskID == "" {
						c.TaskID = entry.TaskID
					}
					break
				}
			}
		}
		captures = append(captures, c)
		return v
	}

	result, err := rewriter.Rewrite(body, contentType, eval)
	if err != nil {
		// Fail closed: the rewriter failed mid-body so we don't know
		// whether a credentialed placeholder survived into the response.
		return failClosed("rewriter error: " + err.Error())
	}

	// Coalesce decision.
	if originalPendingApprovals != nil && shouldCoalesceTurn(captures) {
		coalesced := coalesceFromCaptures(captures)
		coalesced.UserID = cfg.AgentUserID
		coalesced.AgentID = cfg.AgentID
		coalesced.Provider = rewriter.Name()
		coalesced.ConversationID = cfg.ConversationID
		held, holdErr := originalPendingApprovals.Hold(req.Context(), coalesced)
		if holdErr == nil {
			if held.Evicted != nil {
				if cfg.Audit != nil && auditAgent != nil && len(captures) > 0 {
					first := captures[0]
					cfg.Audit.WriteAuditEvent(req.Context(), auditAgent, cfg.RequestID, conversation.AuditEvent{
						ToolUse:          first.Use,
						InspectorVerdict: first.Inspector,
						Decision:         conversation.DecisionBlock,
						OutcomeName:      "approval_evicted",
						Reason:           "superseded pending approval " + held.Evicted.ID,
						TaskID:           first.TaskID,
					})
				}
				llmproxy.CleanupEvictedInlineTask(req.Context(), cfg, held.Evicted)
			}
			emitCoalescedPendingAuditRows(req.Context(), cfg, auditAgent, captures, held.Pending.ID)
			// Re-run the rewriter with a coalesced eval.
			coalescedPrompt := coalescedApprovalPrompt(held.Pending.AllHolds(), held.Pending.ID)
			firstReplaced := false
			coalescedEval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
				out := conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: approval required (coalesced turn) — " + held.Pending.Reason,
				}
				if !firstReplaced {
					out.SubstituteWith = coalescedPrompt
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
			// Coalesced re-run failed; fall through to flush + return first pass.
			flushBufferedAudit(req.Context(), cfg, auditAgent, pendingAuditEvents)
			return llmproxy.PostprocessResult{
				Body:        result.Body,
				ContentType: contentType,
				Rewritten:   result.Rewritten,
				Decisions:   result.Decisions,
			}
		}
		// Hold-failure path: fall through to legacy replay.
	}

	// Legacy replay: no coalescence happened.
	if replayErr := replayBufferedHolds(req.Context(), cfg, originalPendingApprovals, holdSink, auditAgent, captures); replayErr != nil {
		flushBufferedAudit(req.Context(), cfg, auditAgent, pendingAuditEvents)
		return failClosed("approval hold storage failed: " + replayErr.Error())
	}
	flushBufferedAudit(req.Context(), cfg, auditAgent, pendingAuditEvents)

	return llmproxy.PostprocessResult{
		Body:        result.Body,
		ContentType: contentType,
		Rewritten:   result.Rewritten,
		Decisions:   result.Decisions,
	}
}

// selectToolUseEvaluator dispatches to the cfg-supplied
// ToolUseEvaluatorFactory. Nil is a programmer error — the handler
// (and every test that exercises Postprocess) must assign
// pipelineeval.Factory to cfg.ToolUseEvaluatorFactory explicitly.
//
// toolUses is the pre-extracted sibling set when known (buffered
// path); empty for streaming where tool_uses arrive incrementally
// and the factory runs lazily per call.
func selectToolUseEvaluator(req *http.Request, cfg llmproxy.PostprocessConfig, provider conversation.Provider, toolUses []conversation.ToolUse, pendingAuditEvents *pendingAuditEventBuffer) conversation.ToolUseEvaluator {
	if cfg.ToolUseEvaluatorFactory == nil {
		panic("llmproxy/postproc: PostprocessConfig.ToolUseEvaluatorFactory is required — assign pipelineeval.Factory")
	}
	emit := func(ba conversation.AuditEvent) {
		pendingAuditEvents.entries = append(pendingAuditEvents.entries, ba)
	}
	return cfg.ToolUseEvaluatorFactory(req, cfg, provider, toolUses, emit)
}

// coalesceFromCaptures builds the single PendingLiteApproval covering
// every tool_use in a turn. The first approval-needing capture becomes
// the primary; the others become Additional entries. PrimaryIndex
// records where the primary sat in the original turn so AllHolds()
// keeps the model's tool_use order intact.
func coalesceFromCaptures(captures []evalCapture) llmproxy.PendingLiteApproval {
	primaryIdx := -1
	for i, c := range captures {
		if c.Kind == llmproxy.HeldKindApproval {
			primaryIdx = i
			break
		}
	}
	if primaryIdx < 0 {
		primaryIdx = 0
	}
	primary := captures[primaryIdx]
	pending := llmproxy.PendingLiteApproval{
		ToolUse:      primary.Use,
		Inspector:    primary.Inspector,
		Fingerprint:  primary.Fingerprint,
		Reason:       primary.Reason,
		PrimaryIndex: primaryIdx,
	}
	pending.Additional = make([]llmproxy.HeldToolUse, 0, len(captures)-1)
	for i, c := range captures {
		if i == primaryIdx {
			continue
		}
		pending.Additional = append(pending.Additional, llmproxy.HeldToolUse{
			ToolUse:     c.Use,
			Kind:        c.Kind,
			Inspector:   c.Inspector,
			Fingerprint: c.Fingerprint,
			Reason:      c.Reason,
		})
	}
	return pending
}

// coalescedApprovalPrompt renders the prompt for a hold that covers
// multiple tool_uses in a turn. The first held use is named explicitly;
// the rest are summarized as auto-allow / auto-rewrite siblings held
// alongside.
func coalescedApprovalPrompt(uses []llmproxy.HeldToolUse, approvalID string) string {
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
		case llmproxy.HeldKindApproval:
			if reason := strings.TrimSpace(held.Reason); reason != "" {
				b.WriteString(" — approval required: ")
				b.WriteString(reason)
			} else {
				b.WriteString(" — approval required")
			}
		case llmproxy.HeldKindAllow:
			b.WriteString(" — held alongside (would auto-allow on its own)")
		case llmproxy.HeldKindRewrite:
			b.WriteString(" — held alongside (would auto-allow with credential rewrite on its own)")
		}
		if preview := conversation.MakeToolInputPreview(held.ToolUse.Input); preview != "" {
			b.WriteString("\n   Input: ")
			b.WriteString(preview)
		}
	}
	b.WriteString("\n\nReply `yes` or `y` to approve all calls and run them in order, `no` or `n` to deny the whole turn, or `task` to scope this work under a Clawvisor task that covers every call above.")
	b.WriteString(llmproxy.ApprovalIDFooter(approvalID))
	return b.String()
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
