package postproc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// PostprocessStream is the streaming counterpart to Postprocess. It
// wraps the upstream SSE reader, runs the per-tool evaluator chain via
// the registered ToolUseEvaluatorFactory, and emits the rewritten /
// blocked / unchanged stream to w.
func PostprocessStream(
	ctx context.Context,
	req *http.Request,
	r io.Reader,
	w io.Writer,
	contentType string,
	cfg llmproxy.PostprocessConfig,
) (llmproxy.PostprocessResult, error) {
	registry := cfg.ResponseRegistry
	if registry == nil {
		registry = conversation.DefaultResponseRegistry()
	}

	streamingRewriter := matchByRouteStreaming(req, registry)

	// First-turn routing notice. Wrap the destination so the per-event
	// SSE state machine emits through an injector that prepends the
	// notice block at index 0 and shifts the rest by +1.
	if cfg.FirstTurnNotice != "" && streamingRewriter != nil {
		shape := conversation.DetectStreamShape(req, streamingRewriter.Name())
		noticeW := conversation.NewStreamingFirstTurnNoticeWriter(w, shape, cfg.FirstTurnNotice)
		if closer, ok := noticeW.(io.Closer); ok {
			defer func() { _ = closer.Close() }()
		}
		w = noticeW
	}

	if cfg.Inspector == nil {
		_, err := io.Copy(w, r)
		return llmproxy.PostprocessResult{SkippedReason: "no inspector configured"}, err
	}
	if streamingRewriter == nil {
		_, err := io.Copy(w, r)
		return llmproxy.PostprocessResult{SkippedReason: "no streaming rewriter for route"}, err
	}

	provider := streamingRewriter.Name()
	auditAgent := llmproxy.AuditAgentForCfg(cfg)

	originalPendingApprovals := cfg.PendingApprovals
	holdSink := &capturedHoldSink{}
	if originalPendingApprovals != nil {
		cfg.PendingApprovals = newHoldCapturingApprovalCache(originalPendingApprovals, holdSink)
	}
	pendingAuditEvents := &pendingAuditEventBuffer{}
	var captures []evalCapture

	// Streaming rewriter consumes the upstream stream and returns the
	// full tool_use list it observed. We run the response-level
	// orchestrator AFTER StreamRewrite so the factory can pre-run
	// pipeline.EvaluateToolUses once on the full sibling set — same
	// architectural shape as the buffered path. The synthetic events
	// (blocked prompt, rewritten tool_uses) below use the verdict
	// lookup, no per-call pipeline runs.
	streamResult, err := streamingRewriter.StreamRewrite(ctx, r, w)
	if err != nil {
		return llmproxy.PostprocessResult{}, err
	}
	if len(streamResult.ToolUses) == 0 {
		return llmproxy.PostprocessResult{
			ContentType: contentType,
		}, nil
	}

	innerEval := selectToolUseEvaluator(req, cfg, provider, streamResult.ToolUses, pendingAuditEvents)

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
						c.Inspector = llmproxy.InspectorVerdictFromSnapshot(entry.InspectorVerdict)
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

	var decisions []conversation.ToolUseDecisionRecord
	anyBlocked := false
	anyRewritten := false
	rewrittenInput := map[string]json.RawMessage{}

	for _, tu := range streamResult.ToolUses {
		v := eval(tu)
		decisions = append(decisions, conversation.ToolUseDecisionRecord{
			ToolUse:          tu,
			Verdict:          v,
			ToolInputPreview: conversation.MakeToolInputPreview(tu.Input),
		})
		if !v.Allowed {
			anyBlocked = true
		}
		if v.Allowed && len(v.RewriteInput) > 0 {
			rewrittenInput[tu.ID] = v.RewriteInput
			anyRewritten = true
		}
	}

	if originalPendingApprovals != nil && shouldCoalesceTurn(captures) {
		coalesced := coalesceFromCaptures(captures)
		coalesced.UserID = cfg.AgentUserID
		coalesced.AgentID = cfg.AgentID
		coalesced.Provider = provider
		coalesced.ConversationID = cfg.ConversationID
		held, holdErr := originalPendingApprovals.Hold(req.Context(), coalesced)
		if holdErr == nil {
			if held.Evicted != nil {
				if cfg.Audit != nil && auditAgent != nil && len(captures) > 0 {
					first := captures[0]
					cfg.Audit.WriteAuditEvent(req.Context(), auditAgent, cfg.RequestID, conversation.AuditEvent{
						ToolUse:          first.Use,
						InspectorVerdict: llmproxy.InspectorSnapshot(first.Inspector),
						Decision:         conversation.DecisionBlock,
						OutcomeName:      "approval_evicted",
						Reason:           "superseded pending approval " + held.Evicted.ID,
						TaskID:           first.TaskID,
					})
				}
				llmproxy.CleanupEvictedInlineTask(req.Context(), cfg, held.Evicted)
			}
			emitCoalescedPendingAuditRows(req.Context(), cfg, auditAgent, captures, held.Pending.ID)

			coalescedPrompt := coalescedApprovalPrompt(held.Pending.AllHolds(), held.Pending.ID)
			if err := writeProviderBlockedPrompt(w, provider, streamResult, coalescedPrompt, streamingBlockedPromptIndex(provider, streamResult, captures)); err != nil {
				return llmproxy.PostprocessResult{}, err
			}

			return llmproxy.PostprocessResult{
				ContentType: contentType,
				Rewritten:   true,
				Decisions:   decisions,
			}, nil
		}
	}

	if replayErr := replayBufferedHolds(req.Context(), cfg, originalPendingApprovals, holdSink, auditAgent, captures); replayErr != nil {
		flushBufferedAudit(req.Context(), cfg, auditAgent, pendingAuditEvents)
		return llmproxy.PostprocessResult{
			SkippedReason: "approval hold storage failed: " + replayErr.Error(),
		}, nil
	}
	flushBufferedAudit(req.Context(), cfg, auditAgent, pendingAuditEvents)

	var continuationResults []conversation.ContinuationToolResult
	for _, dec := range decisions {
		if dec.Verdict.ContinueWithToolResult != "" {
			continuationResults = append(continuationResults, conversation.ContinuationToolResult{
				ToolUseID: dec.ToolUse.ID,
				Content:   dec.Verdict.ContinueWithToolResult,
			})
		}
	}

	if len(continuationResults) > 0 {
		return llmproxy.PostprocessResult{
			ContentType:             contentType,
			Rewritten:               true,
			Decisions:               decisions,
			ContinuationToolResults: continuationResults,
			AssistantTurn:           streamResult.AssistantTurn,
			StreamingProvider:       provider,
			StreamingResult:         streamResult,
		}, nil
	}

	if anyBlocked {
		subText := conversation.BlockedReasonText(decisions)
		if strings.TrimSpace(subText) == "" {
			subText = "Tool use was blocked by the Clawvisor proxy."
		}
		if err := writeProviderBlockedPrompt(w, provider, streamResult, subText, streamingBlockedPromptIndex(provider, streamResult, captures)); err != nil {
			return llmproxy.PostprocessResult{}, err
		}
	} else {
		if err := writeProviderToolUses(w, provider, streamResult, streamResult.ToolUses, rewrittenInput); err != nil {
			return llmproxy.PostprocessResult{}, err
		}
		if err := writeProviderStop(w, provider, streamResult); err != nil {
			return llmproxy.PostprocessResult{}, err
		}
	}

	return llmproxy.PostprocessResult{
		ContentType: contentType,
		Rewritten:   anyRewritten || anyBlocked,
		Decisions:   decisions,
	}, nil
}

func streamingBlockedPromptIndex(provider conversation.Provider, result conversation.StreamingRewriteResult, captures []evalCapture) int {
	if provider == conversation.ProviderAnthropic && result.NextAnthropicContentIndex > 0 {
		return result.NextAnthropicContentIndex
	}
	return len(captures)
}

func writeProviderBlockedPrompt(w io.Writer, provider conversation.Provider, result conversation.StreamingRewriteResult, text string, contentIndex int) error {
	switch provider {
	case conversation.ProviderAnthropic:
		start := map[string]any{
			"type":  "content_block_start",
			"index": contentIndex,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		}
		if err := writeSSE(w, "content_block_start", start); err != nil {
			return err
		}
		delta := map[string]any{
			"type":  "content_block_delta",
			"index": contentIndex,
			"delta": map[string]any{
				"type": "text_delta",
				"text": text,
			},
		}
		if err := writeSSE(w, "content_block_delta", delta); err != nil {
			return err
		}
		stop := map[string]any{
			"type":  "content_block_stop",
			"index": contentIndex,
		}
		if err := writeSSE(w, "content_block_stop", stop); err != nil {
			return err
		}
		return writeAnthropicStopSSE(w, "end_turn")

	case conversation.ProviderOpenAI:
		if result.StreamFormat == "openai_responses" {
			_, err := w.Write(conversation.SynthOpenAIResponsesTextSSE(text))
			return err
		}
		chunk := map[string]any{
			"id":     firstNonEmpty(result.StreamID, "chatcmpl-clawvisor"),
			"object": "chat.completion.chunk",
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"role":    "assistant",
						"content": text,
					},
				},
			},
		}
		if err := writeOpenAIData(w, chunk); err != nil {
			return err
		}
		stopChunk := map[string]any{
			"id":     firstNonEmpty(result.StreamID, "chatcmpl-clawvisor"),
			"object": "chat.completion.chunk",
			"choices": []any{
				map[string]any{
					"index":         0,
					"finish_reason": "stop",
				},
			},
		}
		if err := writeOpenAIData(w, stopChunk); err != nil {
			return err
		}
		_, err := fmt.Fprint(w, "data: [DONE]\n\n")
		return err
	}
	return nil
}

func writeProviderToolUses(w io.Writer, provider conversation.Provider, result conversation.StreamingRewriteResult, tus []conversation.ToolUse, rewrittenInput map[string]json.RawMessage) error {
	switch provider {
	case conversation.ProviderAnthropic:
		return writeAnthropicToolUsesSSE(w, tus, rewrittenInput)
	case conversation.ProviderOpenAI:
		if result.StreamFormat == "openai_responses" {
			_, err := w.Write(conversation.SynthOpenAIResponsesFunctionCallsSSE(syntheticCallsFromToolUses(tus, rewrittenInput)))
			return err
		}
		return writeOpenAIChatToolUsesSSE(w, result.StreamID, tus, rewrittenInput)
	}
	return nil
}

func writeProviderStop(w io.Writer, provider conversation.Provider, result conversation.StreamingRewriteResult) error {
	switch provider {
	case conversation.ProviderAnthropic:
		return writeAnthropicStopSSE(w, "tool_use")
	case conversation.ProviderOpenAI:
		if result.StreamFormat == "openai_responses" {
			return nil
		}
		chunk := map[string]any{
			"id":     firstNonEmpty(result.StreamID, "chatcmpl-clawvisor"),
			"object": "chat.completion.chunk",
			"choices": []any{
				map[string]any{
					"index":         0,
					"finish_reason": "tool_calls",
				},
			},
		}
		if err := writeOpenAIData(w, chunk); err != nil {
			return err
		}
		_, err := fmt.Fprint(w, "data: [DONE]\n\n")
		return err
	}
	return nil
}

func writeAnthropicToolUsesSSE(w io.Writer, tus []conversation.ToolUse, rewrittenInput map[string]json.RawMessage) error {
	for _, tu := range tus {
		input := tu.Input
		if rw, ok := rewrittenInput[tu.ID]; ok {
			input = rw
		}

		start := map[string]any{
			"type":  "content_block_start",
			"index": tu.Index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    tu.ID,
				"name":  tu.Name,
				"input": map[string]any{},
			},
		}
		if err := writeSSE(w, "content_block_start", start); err != nil {
			return err
		}

		delta := map[string]any{
			"type":  "content_block_delta",
			"index": tu.Index,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": string(input),
			},
		}
		if err := writeSSE(w, "content_block_delta", delta); err != nil {
			return err
		}

		stop := map[string]any{
			"type":  "content_block_stop",
			"index": tu.Index,
		}
		if err := writeSSE(w, "content_block_stop", stop); err != nil {
			return err
		}
	}
	return nil
}

func writeAnthropicStopSSE(w io.Writer, stopReason string) error {
	delta := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": 0},
	}
	if err := writeSSE(w, "message_delta", delta); err != nil {
		return err
	}
	return writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
}

func writeOpenAIChatToolUsesSSE(w io.Writer, streamID string, tus []conversation.ToolUse, rewrittenInput map[string]json.RawMessage) error {
	for _, tu := range tus {
		args := string(tu.Input)
		if rw, ok := rewrittenInput[tu.ID]; ok {
			args = string(rw)
		}
		chunk := map[string]any{
			"id":     firstNonEmpty(streamID, "chatcmpl-clawvisor"),
			"object": "chat.completion.chunk",
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": tu.Index,
								"id":    tu.ID,
								"type":  "function",
								"function": map[string]any{
									"name":      tu.Name,
									"arguments": args,
								},
							},
						},
					},
				},
			},
		}
		if err := writeOpenAIData(w, chunk); err != nil {
			return err
		}
	}
	return nil
}

func syntheticCallsFromToolUses(tus []conversation.ToolUse, rewrittenInput map[string]json.RawMessage) []conversation.SyntheticToolCall {
	calls := make([]conversation.SyntheticToolCall, 0, len(tus))
	for _, tu := range tus {
		input := tu.Input
		if rw, ok := rewrittenInput[tu.ID]; ok {
			input = rw
		}
		var decoded map[string]any
		if len(input) > 0 {
			_ = json.Unmarshal(input, &decoded)
		}
		if decoded == nil {
			decoded = map[string]any{}
		}
		calls = append(calls, conversation.SyntheticToolCall{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: decoded,
		})
	}
	return calls
}

func writeSSE(w io.Writer, event string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", string(raw))
	return err
}

func writeOpenAIData(w io.Writer, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", string(raw))
	return err
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	if len(values) > 0 {
		return values[0]
	}
	return ""
}
