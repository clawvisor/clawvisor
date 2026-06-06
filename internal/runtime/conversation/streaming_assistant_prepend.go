package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// StreamShape identifies which streaming SSE wire format a notice
// writer should target. The shape is fixed at construction so the
// per-event injection logic can branch deterministically without
// sniffing bytes.
type StreamShape int

const (
	StreamShapeUnknown StreamShape = iota
	StreamShapeAnthropicMessages
	StreamShapeOpenAIChat
	StreamShapeOpenAIResponses
	// StreamShapeGoogleGemini is Gemini's :streamGenerateContent SSE
	// shape, with a different envelope than Anthropic or OpenAI.
	StreamShapeGoogleGemini
)

// DetectStreamShape picks the streaming SSE shape from the inbound LLM
// request. Returns StreamShapeUnknown when neither provider matches —
// the streaming-notice writer treats that as "no-op pass-through" so a
// stray shape never blocks a stream.
func DetectStreamShape(req *http.Request, provider Provider) StreamShape {
	switch provider {
	case ProviderAnthropic:
		return StreamShapeAnthropicMessages
	case ProviderOpenAI:
		if IsOpenAIChatCompletionsEndpoint(req) {
			return StreamShapeOpenAIChat
		}
		return StreamShapeOpenAIResponses
	case ProviderGoogle:
		return StreamShapeGoogleGemini
	}
	return StreamShapeUnknown
}

// NewStreamingFirstTurnNoticeWriter wraps dest with an io.Writer that
// transforms the SSE event stream written through it by injecting a
// leading assistant-text notice at output index 0 and shifting all
// subsequent indices by +1. The shape parameter selects the wire format:
//
//   - StreamShapeAnthropicMessages — after message_start passes through,
//     emit content_block_start/delta/stop at index 0 with the notice
//     text; shift `index` on every later content_block_* event by +1.
//   - StreamShapeOpenAIResponses — after response.created passes through
//     (or eagerly at the top if absent), emit the six-event notice
//     envelope at output_index 0; shift `output_index` on every later
//     event by +1; rewrite response.completed to prepend the notice item
//     to response.output[] so reconcilers see the notice in the final
//     state.
//   - StreamShapeOpenAIChat — emit a synthetic chat.completion.chunk
//     carrying role:"assistant" + content:<text> at the top of the
//     stream, then pass everything else through unchanged.
//
// When text is blank or shape is StreamShapeUnknown, returns dest
// unchanged (no-op).
//
// The wrapper line-buffers partial writes — callers don't need to
// align Write boundaries with SSE event boundaries.
func NewStreamingFirstTurnNoticeWriter(dest io.Writer, shape StreamShape, text string) io.Writer {
	if strings.TrimSpace(text) == "" {
		return dest
	}
	if shape == StreamShapeUnknown {
		return dest
	}
	return &streamingPrependWriter{
		dest:  dest,
		shape: shape,
		text:  text,
	}
}

// streamingPrependWriter does the per-event line buffering shared by
// the Anthropic and OpenAI-Responses paths. Each Write may carry
// partial events; we accumulate lines until we hit a blank-line
// terminator, then dispatch one complete event.
type streamingPrependWriter struct {
	dest  io.Writer
	shape StreamShape
	text  string

	// lineBuf holds bytes that haven't yet completed a line (no \n
	// yet). Once a \n arrives the accumulated line is processed.
	lineBuf bytes.Buffer

	// Event accumulator — SSE events are zero or more `event:` /
	// `data:` lines followed by a blank line.
	curEvent string
	dataLns  []string

	// State for index shifting + injection. noticeInjected goes true
	// after we've emitted the leading notice events; from then on we
	// shift downstream indices.
	noticeInjected bool

	// State for Anthropic thinking fallback
	hasSeenAnyBlock bool
	maxIndexSeen    int
}

func (s *streamingPrependWriter) Write(p []byte) (int, error) {
	if _, err := s.lineBuf.Write(p); err != nil {
		return 0, err
	}
	for s.processNextLine() {
	}
	return len(p), nil
}

// Close flushes any partial state the writer has buffered. SSE streams
// SHOULD terminate every event with a blank line; this is the safety
// net for upstreams that don't, and for unit tests that don't end on a
// blank-line boundary. Safe to call multiple times.
func (s *streamingPrependWriter) Close() error {
	// Any line bytes without a trailing newline get treated as a final
	// line so partial events at stream-end still flush.
	if s.lineBuf.Len() > 0 {
		trailing := s.lineBuf.String()
		s.lineBuf.Reset()
		trimmed := strings.TrimRight(trailing, "\r")
		if strings.HasPrefix(trimmed, "event:") {
			s.curEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		} else if strings.HasPrefix(trimmed, "data:") {
			s.dataLns = append(s.dataLns, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		} else if !strings.HasPrefix(trimmed, ":") && trimmed != "" {
			fmt.Fprintln(s.dest, trailing)
		}
	}
	if s.curEvent != "" || len(s.dataLns) > 0 {
		s.flushEvent()
	}
	// Chat fallback — if the stream ended without a mergeable chunk
	// AND without `data: [DONE]`, surface the notice via a synthetic
	// trailing chunk so the user still sees it. Cheap to emit; loose
	// accumulators happily concat it onto the prior assistant turn.
	if s.shape == StreamShapeOpenAIChat && !s.noticeInjected {
		s.emitSyntheticChatNotice()
		s.noticeInjected = true
	}
	// Anthropic fallback — if the stream ended without notice being injected (e.g. thinking-only stream)
	if s.shape == StreamShapeAnthropicMessages && !s.noticeInjected {
		s.injectAnthropicFallbackNotice()
	}
	return nil
}

// processNextLine consumes one complete line (terminated by \n) from
// the line buffer. Returns false when there's no complete line left
// (caller stops looping). Returns true even on emit errors so the
// caller drains the buffer — surfacing write errors mid-stream is a
// non-recoverable case anyway (the client connection is gone).
func (s *streamingPrependWriter) processNextLine() bool {
	data := s.lineBuf.Bytes()
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return false
	}
	line := string(data[:nl])
	s.lineBuf.Next(nl + 1)

	trimmed := strings.TrimRight(line, "\r")

	// Comment line — pass through verbatim (preserves SSE keepalives,
	// vendor pings, etc. that the rewriter forwarded).
	if strings.HasPrefix(trimmed, ":") {
		fmt.Fprintln(s.dest, line)
		return true
	}

	// Blank line — terminates the current event.
	if trimmed == "" {
		s.flushEvent()
		return true
	}

	if strings.HasPrefix(trimmed, "event:") {
		s.curEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		return true
	}
	if strings.HasPrefix(trimmed, "data:") {
		// OpenAI Chat Completions framing is one `data:` per event with
		// blank-line separators that the upstream rewriter doesn't
		// always preserve (the pass-through path in
		// OpenAIResponseRewriter.streamRewriteChatCompletions emits
		// `data: …\n` without the trailing `\n\n`). Treat each `data:`
		// line as terminating the previous chat event so chunks don't
		// silently merge into one giant accumulated event.
		if s.shape == StreamShapeOpenAIChat && len(s.dataLns) > 0 {
			s.flushEvent()
		}
		s.dataLns = append(s.dataLns, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		return true
	}

	// Unrecognized line shape — pass through unchanged so we don't
	// silently drop content. SSE allows id: / retry: lines that we
	// don't model.
	fmt.Fprintln(s.dest, line)
	return true
}

// flushEvent dispatches one complete accumulated event to the
// provider-specific handler and resets the accumulator.
func (s *streamingPrependWriter) flushEvent() {
	if len(s.dataLns) == 0 && s.curEvent == "" {
		return
	}
	event := s.curEvent
	data := strings.Join(s.dataLns, "\n")
	s.curEvent = ""
	s.dataLns = s.dataLns[:0]

	switch s.shape {
	case StreamShapeAnthropicMessages:
		s.flushAnthropic(event, data)
	case StreamShapeOpenAIResponses:
		s.flushOpenAIResponses(event, data)
	case StreamShapeOpenAIChat:
		s.flushOpenAIChat(event, data)
	default:
		// Unknown shape — pass through (defensive; constructor refuses
		// StreamShapeUnknown, but keep this branch correct anyway).
		s.passThrough(event, data)
	}
}

// passThrough emits an event to the destination in canonical
// `event:` / `data:` / blank-line form. Used by the per-shape handlers
// for any event they don't transform.
func (s *streamingPrependWriter) passThrough(event, data string) {
	s.emitEvent(event, data)
}

func (s *streamingPrependWriter) emitEvent(event, data string) {
	if event != "" {
		fmt.Fprintf(s.dest, "event: %s\n", event)
	}
	fmt.Fprintf(s.dest, "data: %s\n\n", data)
}

func (s *streamingPrependWriter) emitJSON(event string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.emitEvent(event, string(raw))
}

// --- Anthropic ---------------------------------------------------------

func (s *streamingPrependWriter) injectAnthropicFallbackNotice() {
	idx := 0
	if s.hasSeenAnyBlock {
		idx = s.maxIndexSeen + 1
	}
	s.emitJSON("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
	s.emitJSON("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{"type": "text_delta", "text": s.text},
	})
	s.emitJSON("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
	s.noticeInjected = true
}

func (s *streamingPrependWriter) flushAnthropic(event, data string) {
	switch event {
	case "message_start":
		s.passThrough(event, data)

	case "content_block_start":
		if !s.noticeInjected {
			var cbs struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
				} `json:"content_block"`
			}
			err := json.Unmarshal([]byte(data), &cbs)
			if err == nil {
				s.hasSeenAnyBlock = true
				if cbs.Index > s.maxIndexSeen {
					s.maxIndexSeen = cbs.Index
				}
				if cbs.ContentBlock.Type == "thinking" || cbs.ContentBlock.Type == "redacted_thinking" {
					// Keep thinking block at index 0, do not inject notice yet.
					s.passThrough(event, data)
					return
				}
			}

			// We need to inject the notice block at the current index.
			idx := 0
			if err == nil {
				idx = cbs.Index
			}

			s.emitJSON("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
			s.emitJSON("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]any{"type": "text_delta", "text": s.text},
			})
			s.emitJSON("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": idx,
			})
			s.noticeInjected = true

			// Shift the current block by 1 and emit.
			shifted, ok := shiftAnthropicEventIndex(event, data, 1)
			if !ok {
				s.passThrough(event, data)
				return
			}
			s.emitEvent(event, string(shifted))
			return
		}

		// Notice is already injected. Shift the current block by 1.
		shifted, ok := shiftAnthropicEventIndex(event, data, 1)
		if !ok {
			s.passThrough(event, data)
			return
		}
		s.emitEvent(event, string(shifted))

	case "content_block_delta", "content_block_stop":
		if !s.noticeInjected {
			s.passThrough(event, data)
			return
		}
		shifted, ok := shiftAnthropicEventIndex(event, data, 1)
		if !ok {
			s.passThrough(event, data)
			return
		}
		s.emitEvent(event, string(shifted))

	case "message_delta", "message_stop":
		if !s.noticeInjected {
			s.injectAnthropicFallbackNotice()
		}
		s.passThrough(event, data)

	default:
		s.passThrough(event, data)
	}
}

// --- OpenAI Responses --------------------------------------------------

func (s *streamingPrependWriter) flushOpenAIResponses(event, data string) {
	switch event {
	case "response.created":
		s.passThrough(event, data)
		if !s.noticeInjected {
			s.emitOpenAIResponsesNotice()
			s.noticeInjected = true
		}
		return
	case "response.completed":
		// Late injection — emit the notice envelope first, then the
		// rewritten completion. Mirrors the buffered helper's fallback
		// when response.created was missing.
		if !s.noticeInjected {
			s.emitOpenAIResponsesNotice()
			s.noticeInjected = true
		}
		rewritten, ok := injectNoticeIntoResponsesCompleted(data, s.text)
		if ok {
			s.emitEvent(event, string(rewritten))
			return
		}
		s.passThrough(event, data)
		return
	}

	if !s.noticeInjected {
		// We received a non-response.created/completed event without
		// having injected the notice. Emit eagerly so the harness sees
		// the notice in the same stream.
		s.emitOpenAIResponsesNotice()
		s.noticeInjected = true
	}

	shifted, ok := shiftOpenAIResponsesEventIndex(data, 1)
	if !ok {
		s.passThrough(event, data)
		return
	}
	s.emitEvent(event, string(shifted))
}

func (s *streamingPrependWriter) emitOpenAIResponsesNotice() {
	const noticeItemID = "msg_clawvisor_notice"
	s.emitJSON("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item": map[string]any{
			"type":   "message",
			"id":     noticeItemID,
			"role":   "assistant",
			"status": "in_progress",
		},
	})
	s.emitJSON("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       noticeItemID,
		"output_index":  0,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": ""},
	})
	s.emitJSON("response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       noticeItemID,
		"output_index":  0,
		"content_index": 0,
		"delta":         s.text,
	})
	s.emitJSON("response.output_text.done", map[string]any{
		"type":          "response.output_text.done",
		"item_id":       noticeItemID,
		"output_index":  0,
		"content_index": 0,
		"text":          s.text,
	})
	s.emitJSON("response.content_part.done", map[string]any{
		"type":          "response.content_part.done",
		"item_id":       noticeItemID,
		"output_index":  0,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": s.text},
	})
	s.emitJSON("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": 0,
		"item": map[string]any{
			"type":   "message",
			"id":     noticeItemID,
			"role":   "assistant",
			"status": "completed",
			"content": []map[string]any{
				{"type": "output_text", "text": s.text},
			},
		},
	})
}

// --- OpenAI Chat Completions ------------------------------------------

func (s *streamingPrependWriter) flushOpenAIChat(event, data string) {
	// `data: [DONE]` is the chat stream's terminal sentinel. If we
	// haven't yet found a chunk to merge into, fall back to a
	// synthetic leading chunk so the notice still surfaces. Mirrors
	// synthLeadingNoticeChatSSE in the buffered helper.
	if data == "[DONE]" {
		if !s.noticeInjected {
			s.emitSyntheticChatNotice()
			s.noticeInjected = true
		}
		s.emitEvent(event, data)
		return
	}
	if s.noticeInjected {
		s.emitEvent(event, data)
		return
	}
	// Merge into the first chunk that carries a choices[].delta. The
	// buffered helper goes to lengths to avoid emitting a separate
	// role-bearing chunk in front of the upstream's first chunk
	// because strict accumulators interpret the second `role:"assistant"`
	// as a new assistant turn. Streaming has the same constraint —
	// merge instead of prefix.
	if merged, ok := mergeOpenAIChatChunkWithNotice([]byte(data), s.text); ok {
		s.emitEvent(event, string(merged))
		s.noticeInjected = true
		return
	}
	// This chunk wasn't mergeable (no choices/delta shape). Pass it
	// through and try the next one.
	s.emitEvent(event, data)
}

func (s *streamingPrependWriter) emitSyntheticChatNotice() {
	block := chatCompletionSSEBlock(map[string]any{
		"id":     "chatcmpl_clawvisor_notice",
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": s.text},
			"finish_reason": nil,
		}},
	})
	_, _ = io.WriteString(s.dest, block)
}
