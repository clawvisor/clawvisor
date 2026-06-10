package postproc

import (
	"context"
	"encoding/json"
	"errors"
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

	session := newPostprocessSession(cfg)

	// Streaming rewriter consumes the upstream stream, invokes
	// onToolUse for each tool_use as it completes, and returns the
	// per-stream summary. We collect tool_uses incrementally via the
	// callback so the orchestrator sees them as they're parsed; the
	// factory still pre-runs pipeline.EvaluateToolUses once on the
	// full sibling set after stream end (response-level orchestration
	// gates on the complete list for coalesce decisions).
	var streamedToolUses []conversation.ToolUse
	onToolUse := func(tu conversation.ToolUse) {
		streamedToolUses = append(streamedToolUses, tu)
	}
	streamResult, err := streamingRewriter.StreamRewrite(ctx, r, w, onToolUse)
	if err != nil {
		// StreamRewrite failed before the eval phase, so no holds or
		// pending inline tasks have been created yet. Feeding partial
		// tool_uses with no verdict map would only misclassify cleanup
		// state as hard-deny captures.
		return llmproxy.PostprocessResult{
			ContentType:       contentType,
			StreamingProvider: provider,
			StreamingResult:   streamResult,
		}, err
	}
	// Prefer the incrementally-collected tool_uses from the
	// onToolUse callback. Result.ToolUses stays available as a
	// fallback for any legacy streaming rewriter that doesn't fire
	// the callback (none today, but the interface allows it).
	toolUses := streamedToolUses
	if len(toolUses) == 0 {
		toolUses = streamResult.ToolUses
	}
	if len(toolUses) == 0 {
		return llmproxy.PostprocessResult{
			ContentType: contentType,
		}, nil
	}

	innerEval := session.evaluator(req, provider, toolUses)

	verdictByTU := make(map[string]conversation.ToolUseVerdict, len(toolUses))
	eval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		v := innerEval(tu)
		verdictByTU[tu.ID] = v
		return v
	}

	var decisions []conversation.ToolUseDecisionRecord
	anyBlocked := false
	anyRewritten := false
	rewrittenInput := map[string]json.RawMessage{}

	for _, tu := range toolUses {
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

	finalResult, finalErr := session.finalize(req.Context(), toolUses, verdictByTU)
	if finalErr != nil {
		session.rollback(req.Context(), toolUses, verdictByTU)
		err := finalErr
		return llmproxy.PostprocessResult{
			SkippedReason: err.Error(),
		}, err
	}

	if finalResult.Coalesced {
		if err := writeProviderBlockedPrompt(w, provider, streamResult, finalResult.CoalescedPrompt, streamingBlockedPromptIndex(provider, streamResult, len(toolUses))); err != nil {
			dropErr := session.dropCommittedAndRollback(req.Context(), finalResult.CoalescedCapture)
			if dropErr != nil {
				return llmproxy.PostprocessResult{}, fmt.Errorf("coalesced approval prompt write failed: %w", errors.Join(err, fmt.Errorf("rollback failed: %w", dropErr)))
			}
			return llmproxy.PostprocessResult{}, err
		}
		return llmproxy.PostprocessResult{
			ContentType: contentType,
			Rewritten:   true,
			Decisions:   decisions,
		}, nil
	}

	var continuationResults []conversation.ContinuationToolResult
	for _, dec := range decisions {
		if content, ok := dec.Verdict.ContinuationToolResultContent(); ok {
			continuationResults = append(continuationResults, conversation.ContinuationToolResult{
				ToolUseID: dec.ToolUse.ID,
				Content:   content,
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
		// Tool-call substitution path: when every blocked decision
		// supplies a SubstituteWithToolCall, the streaming codec emits
		// those tool_use blocks directly instead of a blocked-prompt
		// text block. The inline-approval flow uses this to surface
		// the yes/no via AskUserQuestion's native picker UI when
		// AskUserQuestion is in the agent's declared tool list.
		//
		// Mixed shapes (some decisions have SubstituteWithToolCall and
		// others don't) fall through to the legacy text path so we
		// don't accidentally hide a separate refusal under an
		// AskUserQuestion call the user couldn't act on.
		if substBlocks, allHaveToolCall := substituteToolCallsForBlocked(decisions); allHaveToolCall && len(substBlocks) > 0 {
			if err := writeProviderSubstituteToolCalls(w, provider, streamResult, substBlocks); err != nil {
				if dropErr := session.dropAllCommittedAndRollback(req.Context()); dropErr != nil {
					return llmproxy.PostprocessResult{}, fmt.Errorf("substitute tool_call write failed: %w", errors.Join(err, fmt.Errorf("rollback failed: %w", dropErr)))
				}
				return llmproxy.PostprocessResult{}, err
			}
			return llmproxy.PostprocessResult{
				ContentType: contentType,
				Rewritten:   true,
				Decisions:   decisions,
			}, nil
		}
		subText := conversation.BlockedReasonText(decisions)
		if strings.TrimSpace(subText) == "" {
			subText = "Tool use was blocked by the Clawvisor proxy."
		}
		if err := writeProviderBlockedPrompt(w, provider, streamResult, subText, streamingBlockedPromptIndex(provider, streamResult, len(toolUses))); err != nil {
			if dropErr := session.dropAllCommittedAndRollback(req.Context()); dropErr != nil {
				return llmproxy.PostprocessResult{}, fmt.Errorf("blocked prompt write failed: %w", errors.Join(err, fmt.Errorf("rollback failed: %w", dropErr)))
			}
			return llmproxy.PostprocessResult{}, err
		}
	} else {
		if err := writeProviderToolUses(w, provider, streamResult, toolUses, rewrittenInput); err != nil {
			if dropErr := session.dropAllCommittedAndRollback(req.Context()); dropErr != nil {
				return llmproxy.PostprocessResult{}, fmt.Errorf("tool_use write failed: %w", errors.Join(err, fmt.Errorf("rollback failed: %w", dropErr)))
			}
			return llmproxy.PostprocessResult{}, err
		}
		if err := writeProviderStop(w, provider, streamResult); err != nil {
			if dropErr := session.dropAllCommittedAndRollback(req.Context()); dropErr != nil {
				return llmproxy.PostprocessResult{}, fmt.Errorf("stop write failed: %w", errors.Join(err, fmt.Errorf("rollback failed: %w", dropErr)))
			}
			return llmproxy.PostprocessResult{}, err
		}
	}

	return llmproxy.PostprocessResult{
		ContentType: contentType,
		Rewritten:   anyRewritten || anyBlocked,
		Decisions:   decisions,
	}, nil
}

// WriteStreamError appends a provider-shaped terminal error to an
// already-started stream. It is used only after headers/body bytes have
// been committed, where the handler can no longer send a normal HTTP
// error response.
func WriteStreamError(w io.Writer, req *http.Request, provider conversation.Provider, result conversation.StreamingRewriteResult, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	switch provider {
	case conversation.ProviderAnthropic:
		body, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "stream_interrupted",
				"message": message,
			},
			"message_id": firstNonEmptyStreamValue(result.StreamID, "msg_clawvisor_stream_error"),
			"model":      firstNonEmptyStreamValue(result.Model, "unknown"),
		})
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", body)
	case conversation.ProviderOpenAI:
		if result.StreamFormat == "openai_chat" || conversation.IsOpenAIChatCompletionsEndpoint(req) {
			id := firstNonEmptyStreamValue(result.StreamID, "chatcmpl_clawvisor_stream_error")
			model := firstNonEmptyStreamValue(result.Model, "clawvisor-stream-error")
			writeOpenAIChatChunk(w, id, model, map[string]any{"role": "assistant"}, nil)
			writeOpenAIChatChunk(w, id, model, map[string]any{"content": message}, nil)
			writeOpenAIChatChunk(w, id, model, map[string]any{}, "stop")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		id := firstNonEmptyStreamValue(result.StreamID, "resp_clawvisor_stream_error")
		body, _ := json.Marshal(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id":     id,
				"status": "failed",
				"error": map[string]any{
					"type":    "stream_interrupted",
					"message": message,
				},
			},
		})
		_, _ = fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", body)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	default:
		for _, line := range strings.Split(message, "\n") {
			_, _ = fmt.Fprintf(w, ": %s\n", line)
		}
		_, _ = io.WriteString(w, "\n")
	}
}

func writeOpenAIChatChunk(w io.Writer, id, model string, delta map[string]any, finish any) {
	body, _ := json.Marshal(map[string]any{
		"id":     id,
		"object": "chat.completion.chunk",
		"model":  model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
}

func firstNonEmptyStreamValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func streamingBlockedPromptIndex(provider conversation.Provider, result conversation.StreamingRewriteResult, captureCount int) int {
	if provider == conversation.ProviderAnthropic && result.NextAnthropicContentIndex >= 0 {
		// Anthropic's stream parser always returns the next content
		// index; 0 is a valid index when the response contained only
		// tool_use blocks before the blocked prompt.
		return result.NextAnthropicContentIndex
	}
	return captureCount
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

// substitutedBlock pairs a blocked decision's SubstituteWith preamble
// text with its SubstituteWithToolCall payload. The streaming codec
// emits the text content_block first so the harness surfaces the
// approval-prompt body in chat, then the tool_use content_block so
// the picker UI opens for the user's yes/no choice. Text may be ""
// when the decision intentionally only ships a tool_use.
type substitutedBlock struct {
	Text string
	Call conversation.SyntheticToolCall
}

// substituteToolCallsForBlocked walks the blocked decisions and
// pairs each SubstituteWithToolCall payload with its decision's
// SubstituteWith preamble text. The allHaveToolCall return is true
// only when EVERY blocked decision supplies a USABLE
// SubstituteWithToolCall (non-empty Name and a Marshal-able Input)
// — partial coverage, or a malformed call that can't be marshaled,
// fall back to the text path so the transport-level behavior matches
// the buffered Anthropic rewriter (which also falls back to text on
// the same invariants — see anthropicSubstituteToolUseBlock).
func substituteToolCallsForBlocked(decisions []conversation.ToolUseDecisionRecord) (blocks []substitutedBlock, allHaveToolCall bool) {
	allHaveToolCall = true
	for _, dec := range decisions {
		if dec.Verdict.Allowed {
			continue
		}
		call := dec.Verdict.SubstituteWithToolCall
		if call == nil || !canRenderSyntheticToolCall(call) {
			allHaveToolCall = false
			continue
		}
		blocks = append(blocks, substitutedBlock{
			Text: dec.Verdict.SubstituteWith,
			Call: *call,
		})
	}
	return blocks, allHaveToolCall
}

// canRenderSyntheticToolCall mirrors the buffered-path invariant in
// anthropicSubstituteToolUseBlock: a usable substitute call needs a
// non-empty Name, a non-empty ID (correlation key for the eventual
// tool_result — fallback IDs alias multiple substitutions on the
// same turn), and Input that round-trips through json.Marshal.
// Keeping the two transports' validity gates in sync prevents the
// same blocked decision from rendering as text in the buffered
// response yet as a tool_use in the streaming response — which
// would surface inconsistent approval UX depending on whether the
// upstream replied with text/event-stream or application/json.
func canRenderSyntheticToolCall(call *conversation.SyntheticToolCall) bool {
	if call == nil {
		return false
	}
	if strings.TrimSpace(call.Name) == "" || call.ID == "" {
		return false
	}
	input := call.Input
	if input == nil {
		input = map[string]any{}
	}
	if _, err := json.Marshal(input); err != nil {
		return false
	}
	return true
}

// writeProviderSubstituteToolCalls emits, per blocked decision, an
// optional preamble text content_block followed by the synthetic
// tool_use content_block — using the same continuation shape as
// writeProviderBlockedPrompt: just content_block_* events plus a
// trailing message_delta/message_stop. The upstream message_start
// was already forwarded by StreamRewrite, so we do NOT emit a new
// message_start — that would double-up the message envelope.
//
// Only the Anthropic provider is wired today; OpenAI callers fall
// back to the text path via the gate in PostprocessStream.
func writeProviderSubstituteToolCalls(w io.Writer, provider conversation.Provider, result conversation.StreamingRewriteResult, blocks []substitutedBlock) error {
	switch provider {
	case conversation.ProviderAnthropic:
		// Index starts after any pass-through text/thinking blocks
		// the upstream stream already wrote to the client. The
		// blocked-prompt path uses the same source.
		idx := result.NextAnthropicContentIndex
		if idx < 0 {
			idx = 0
		}
		for _, blk := range blocks {
			if strings.TrimSpace(blk.Text) != "" {
				if err := writeAnthropicTextBlock(w, idx, blk.Text); err != nil {
					return err
				}
				idx++
			}
			if err := writeAnthropicToolUseBlock(w, idx, blk.Call); err != nil {
				return err
			}
			idx++
		}
		return writeAnthropicStopSSE(w, "tool_use")
	}
	return nil
}

// writeAnthropicTextBlock emits one text content_block (start +
// delta + stop) at the given index. Shared between
// writeProviderBlockedPrompt and writeProviderSubstituteToolCalls.
func writeAnthropicTextBlock(w io.Writer, index int, text string) error {
	if err := writeSSE(w, "content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	}); err != nil {
		return err
	}
	if err := writeSSE(w, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	}); err != nil {
		return err
	}
	return writeSSE(w, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": index,
	})
}

// writeAnthropicToolUseBlock emits one tool_use content_block (start
// + input_json_delta + stop) at the given index. Used to surface a
// synthetic tool_use (e.g. AskUserQuestion) as part of a substituted
// assistant turn.
func writeAnthropicToolUseBlock(w io.Writer, index int, call conversation.SyntheticToolCall) error {
	inputJSON, err := json.Marshal(call.Input)
	if err != nil {
		inputJSON = []byte("{}")
	}
	if err := writeSSE(w, "content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Name,
			"input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	if err := writeSSE(w, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": string(inputJSON),
		},
	}); err != nil {
		return err
	}
	return writeSSE(w, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": index,
	})
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
