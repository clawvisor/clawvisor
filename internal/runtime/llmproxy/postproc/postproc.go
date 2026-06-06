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
	auditSink := &capturedAuditSink{}
	var captures []evalCapture

	innerEval := selectToolUseEvaluator(req, cfg, rewriter.Name(), auditSink)

	// Outer eval wraps innerEval and records the kind + decision
	// context for the coalesce post-pass. Two side channels feed the
	// capture: holdSink.holds (set by the buffered PendingApprovals
	// wrapper) and auditSink.entries (set by the buffered audit
	// closure). The last entry for this call carries the inspector
	// verdict and final reason even when no hold was created.
	eval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		holdsBefore, auditsBefore := 0, 0
		if holdSink != nil {
			holdsBefore = len(holdSink.holds)
		}
		if auditSink != nil {
			auditsBefore = len(auditSink.entries)
		}
		v := innerEval(tu)
		c := evalCapture{Use: tu, Kind: classifyVerdict(v)}
		if holdSink != nil && len(holdSink.holds) > holdsBefore {
			h := holdSink.holds[len(holdSink.holds)-1]
			c.HoldID = h.Pending.ID
			c.Stage = h.Pending.Stage
			c.Inspector = h.Pending.Inspector
			c.Fingerprint = h.Pending.Fingerprint
			c.Reason = h.Pending.Reason
		} else if auditSink != nil && len(auditSink.entries) > auditsBefore {
			last := auditSink.entries[len(auditSink.entries)-1]
			c.Inspector = last.Verdict
			c.Reason = last.Reason
		}
		if auditSink != nil && len(auditSink.entries) > auditsBefore {
			c.TaskID = auditSink.entries[len(auditSink.entries)-1].TaskID
		}
		captures = append(captures, c)
		return v
	}
	failClosed := func(reason string) llmproxy.PostprocessResult {
		rollbackBufferedPendingTasks(req.Context(), cfg, holdSink)
		return llmproxy.PostprocessResult{
			Body:          nil,
			ContentType:   contentType,
			SkippedReason: reason,
		}
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
					cfg.Audit.LogToolUseInspected(req.Context(), auditAgent, cfg.RequestID, first.Use, first.Inspector, "block", "approval_evicted", "superseded pending approval "+held.Evicted.ID, first.TaskID)
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
			flushBufferedAudit(req.Context(), cfg, auditAgent, auditSink)
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
		flushBufferedAudit(req.Context(), cfg, auditAgent, auditSink)
		return failClosed("approval hold storage failed: " + replayErr.Error())
	}
	flushBufferedAudit(req.Context(), cfg, auditAgent, auditSink)

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
func selectToolUseEvaluator(req *http.Request, cfg llmproxy.PostprocessConfig, provider conversation.Provider, auditSink *capturedAuditSink) conversation.ToolUseEvaluator {
	if cfg.ToolUseEvaluatorFactory == nil {
		panic("llmproxy/postproc: PostprocessConfig.ToolUseEvaluatorFactory is required — assign pipelineeval.Factory")
	}
	emit := func(ba llmproxy.BufferedAudit) {
		auditSink.entries = append(auditSink.entries, ba)
	}
	return cfg.ToolUseEvaluatorFactory(req, cfg, provider, emit)
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
