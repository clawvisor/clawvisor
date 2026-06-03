package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// PrependAnthropicAssistantText inserts a text block at the start of
// an Anthropic /v1/messages assistant response. Used by the lite-proxy
// continuation path to surface a Clawvisor notice ("a task was
// auto-approved") to the user in the same turn as the model's next
// actions. The original response's id, model, stop_reason, usage, and
// every content block (text + tool_use) are preserved.
//
// Supports both JSON and SSE wire formats. Returns the original body
// untouched on any parse error so a malformed upstream response
// doesn't strand the harness with an empty body.
func PrependAnthropicAssistantText(contentType string, body []byte, text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return body, nil
	}
	if isSSE(contentType) {
		return prependAnthropicAssistantTextSSE(body, text)
	}
	return prependAnthropicAssistantTextJSON(body, text)
}

func prependAnthropicAssistantTextJSON(body []byte, text string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, nil
	}
	contentRaw, ok := top["content"]
	if !ok {
		// Synthesize a content[] array with just our text block. Some
		// edge-case Anthropic shapes (count_tokens, error envelopes)
		// don't carry content[]; treat them as no-op rather than
		// inventing fields.
		return body, nil
	}
	var content []json.RawMessage
	if err := json.Unmarshal(contentRaw, &content); err != nil {
		return body, nil
	}
	textBlock, err := json.Marshal(map[string]any{"type": "text", "text": text})
	if err != nil {
		return nil, fmt.Errorf("prepend anthropic text: marshal text block: %w", err)
	}
	// Inspect the first content block. If it is a thinking block or redacted_thinking
	// block, we insert the notice block at index 1 instead of index 0.
	isThinking := false
	if len(content) > 0 {
		var firstBlock struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(content[0], &firstBlock); err == nil {
			if firstBlock.Type == "thinking" || firstBlock.Type == "redacted_thinking" {
				isThinking = true
			}
		}
	}

	// Use append-from-literal rather than make-with-precomputed-cap.
	// CodeQL flags `len(content)+1` as a potential overflow; the input
	// is bounded by Anthropic content-block limits so it's unreachable
	// in practice, but sidestepping the explicit arithmetic keeps the
	// static analyzer quiet.
	var merged []json.RawMessage
	if isThinking {
		merged = append([]json.RawMessage{content[0], json.RawMessage(textBlock)}, content[1:]...)
	} else {
		merged = append([]json.RawMessage{json.RawMessage(textBlock)}, content...)
	}
	mergedRaw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("prepend anthropic text: marshal content: %w", err)
	}
	top["content"] = mergedRaw
	out, err := json.Marshal(top)
	if err != nil {
		return nil, fmt.Errorf("prepend anthropic text: marshal envelope: %w", err)
	}
	return out, nil
}

// prependAnthropicAssistantTextSSE walks the SSE event stream and
// injects a text block at index 0, shifting all subsequent
// content_block_start / content_block_delta / content_block_stop
// indices by +1. message_start, message_delta, message_stop, and any
// unrelated events pass through unchanged.
//
// Strategy is stream-edit (not full re-emit) so non-tool-related
// upstream events the rewriter doesn't model (ping, errors, vendor
// extensions) aren't silently dropped.
func prependAnthropicAssistantTextSSE(body []byte, text string) ([]byte, error) {
	events, err := parseSSEEvents(body)
	if err != nil {
		return body, nil
	}

	var out bytes.Buffer
	emit := func(name string, data any) error {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		out.WriteString("event: ")
		out.WriteString(name)
		out.WriteString("\ndata: ")
		out.Write(raw)
		out.WriteString("\n\n")
		return nil
	}

	delayNotice := isFirstBlockThinking(events)
	noticeInserted := false
	maxIndexSeen := 0

	emitNotice := func(idx int) error {
		if err := emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		}); err != nil {
			return err
		}
		if err := emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{"type": "text_delta", "text": text},
		}); err != nil {
			return err
		}
		if err := emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		}); err != nil {
			return err
		}
		noticeInserted = true
		return nil
	}

	for _, ev := range events {
		switch ev.Event {
		case "message_start":
			// Pass through verbatim.
			out.WriteString("event: ")
			out.WriteString(ev.Event)
			out.WriteString("\ndata: ")
			out.WriteString(ev.Data)
			out.WriteString("\n\n")

			if !delayNotice {
				if err := emitNotice(0); err != nil {
					return body, nil
				}
			}
		case "content_block_start":
			var cbs struct {
				Index int `json:"index"`
			}
			_ = json.Unmarshal([]byte(ev.Data), &cbs)
			if cbs.Index > maxIndexSeen {
				maxIndexSeen = cbs.Index
			}

			if delayNotice {
				if !noticeInserted {
					if cbs.Index == 0 {
						// Pass through the thinking block verbatim.
						out.WriteString("event: ")
						out.WriteString(ev.Event)
						out.WriteString("\ndata: ")
						out.WriteString(ev.Data)
						out.WriteString("\n\n")
					} else {
						// Inject notice block at index 1
						if err := emitNotice(1); err != nil {
							return body, nil
						}
						// Shift this block to index 2
						shifted, ok := shiftAnthropicEventIndex(ev.Event, ev.Data, 1)
						if !ok {
							return body, nil
						}
						out.WriteString("event: ")
						out.WriteString(ev.Event)
						out.WriteString("\ndata: ")
						out.Write(shifted)
						out.WriteString("\n\n")
					}
				} else {
					shifted, ok := shiftAnthropicEventIndex(ev.Event, ev.Data, 1)
					if !ok {
						return body, nil
					}
					out.WriteString("event: ")
					out.WriteString(ev.Event)
					out.WriteString("\ndata: ")
					out.Write(shifted)
					out.WriteString("\n\n")
				}
			} else {
				if !noticeInserted {
					// No message_start observed yet. Pass through unchanged.
					out.WriteString("event: ")
					out.WriteString(ev.Event)
					out.WriteString("\ndata: ")
					out.WriteString(ev.Data)
					out.WriteString("\n\n")
					continue
				}
				shifted, ok := shiftAnthropicEventIndex(ev.Event, ev.Data, 1)
				if !ok {
					return body, nil
				}
				out.WriteString("event: ")
				out.WriteString(ev.Event)
				out.WriteString("\ndata: ")
				out.Write(shifted)
				out.WriteString("\n\n")
			}
		case "content_block_delta", "content_block_stop":
			if delayNotice {
				if !noticeInserted {
					// Must be index 0 (thinking block). Pass through verbatim.
					out.WriteString("event: ")
					out.WriteString(ev.Event)
					out.WriteString("\ndata: ")
					out.WriteString(ev.Data)
					out.WriteString("\n\n")
				} else {
					shifted, ok := shiftAnthropicEventIndex(ev.Event, ev.Data, 1)
					if !ok {
						return body, nil
					}
					out.WriteString("event: ")
					out.WriteString(ev.Event)
					out.WriteString("\ndata: ")
					out.Write(shifted)
					out.WriteString("\n\n")
				}
			} else {
				if !noticeInserted {
					out.WriteString("event: ")
					out.WriteString(ev.Event)
					out.WriteString("\ndata: ")
					out.WriteString(ev.Data)
					out.WriteString("\n\n")
					continue
				}
				shifted, ok := shiftAnthropicEventIndex(ev.Event, ev.Data, 1)
				if !ok {
					return body, nil
				}
				out.WriteString("event: ")
				out.WriteString(ev.Event)
				out.WriteString("\ndata: ")
				out.Write(shifted)
				out.WriteString("\n\n")
			}
		case "message_delta", "message_stop":
			if delayNotice && !noticeInserted {
				// Inject notice block at index 1 before message delta/stop
				if err := emitNotice(1); err != nil {
					return body, nil
				}
			}
			out.WriteString("event: ")
			out.WriteString(ev.Event)
			out.WriteString("\ndata: ")
			out.WriteString(ev.Data)
			out.WriteString("\n\n")
		default:
			out.WriteString("event: ")
			out.WriteString(ev.Event)
			out.WriteString("\ndata: ")
			out.WriteString(ev.Data)
			out.WriteString("\n\n")
		}
	}

	if delayNotice && !noticeInserted {
		if err := emitNotice(maxIndexSeen + 1); err != nil {
			return body, nil
		}
	}
	return out.Bytes(), nil
}

// PrependOpenAIChatAssistantText inserts a leading text content into
// an OpenAI Chat Completions response. JSON: prepends to
// choices[0].message.content (handling string / null / blocks). SSE:
// emits a role+content delta pair at the top of the stream carrying
// the notice, then passes through the upstream's deltas.
func PrependOpenAIChatAssistantText(contentType string, body []byte, text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return body, nil
	}
	if isSSE(contentType) || looksLikeSSE(body) {
		return prependOpenAIChatAssistantTextSSE(body, text)
	}
	return prependOpenAIChatAssistantTextJSON(body, text)
}

func prependOpenAIChatAssistantTextJSON(body []byte, text string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, nil
	}
	choicesRaw, ok := top["choices"]
	if !ok {
		return body, nil
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(choicesRaw, &choices); err != nil {
		return body, nil
	}
	if len(choices) == 0 {
		return body, nil
	}
	msgRaw, ok := choices[0]["message"]
	if !ok {
		return body, nil
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return body, nil
	}
	// Combine notice + existing content. Chat Completions accepts
	// content as null (when only tool_calls is present), a string, or
	// a content-parts array. We collapse to a string in the null /
	// missing / string cases (preserves the simpler shape most
	// harnesses produce); blocks case prepends a text block.
	contentRaw, hasContent := msg["content"]
	switch {
	case !hasContent, len(contentRaw) == 0, string(contentRaw) == "null":
		raw, err := json.Marshal(text)
		if err != nil {
			return nil, fmt.Errorf("prepend openai chat: marshal text: %w", err)
		}
		msg["content"] = raw
	default:
		var asString string
		if err := json.Unmarshal(contentRaw, &asString); err == nil {
			raw, err := json.Marshal(text + "\n\n" + asString)
			if err != nil {
				return nil, fmt.Errorf("prepend openai chat: marshal string content: %w", err)
			}
			msg["content"] = raw
			break
		}
		var blocks []json.RawMessage
		if err := json.Unmarshal(contentRaw, &blocks); err != nil {
			return body, nil
		}
		textBlock, err := json.Marshal(map[string]any{"type": "text", "text": text})
		if err != nil {
			return nil, fmt.Errorf("prepend openai chat: marshal text block: %w", err)
		}
		merged := append([]json.RawMessage{json.RawMessage(textBlock)}, blocks...)
		raw, err := json.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("prepend openai chat: marshal blocks: %w", err)
		}
		msg["content"] = raw
	}
	msgMarshaled, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("prepend openai chat: marshal message: %w", err)
	}
	choices[0]["message"] = msgMarshaled
	choicesMarshaled, err := json.Marshal(choices)
	if err != nil {
		return nil, fmt.Errorf("prepend openai chat: marshal choices: %w", err)
	}
	top["choices"] = choicesMarshaled
	out, err := json.Marshal(top)
	if err != nil {
		return nil, fmt.Errorf("prepend openai chat: marshal envelope: %w", err)
	}
	return out, nil
}

func prependOpenAIChatAssistantTextSSE(body []byte, text string) ([]byte, error) {
	// Rewrite the FIRST chunk's delta to carry our notice as the
	// `content` field, preserving any role / tool_calls already
	// present on that delta. Emitting a separate role chunk in front
	// of the upstream's role chunk causes strict accumulators to
	// interpret the two role transitions as two distinct assistant
	// turns — rendering the notice as a separate message instead of
	// merging with the model's output. Merging into the first chunk
	// avoids the double-role transition entirely.
	//
	// Use parseSSEEvents (the buffered scanner the other SSE helpers
	// in this file use) rather than strings.Split. The split approach
	// (a) materializes the whole body as a string plus a []string of
	// every line, ~2-3x the input size, and (b) re-emitted the
	// rewritten chunk with its own \n\n then let the original blank
	// separator line add another \n, producing triple-newline framing
	// that spec-strict consumers reject. The scanner already handles
	// the line/blank/event grouping correctly; we re-emit each event
	// with canonical "data: <payload>\n\n" framing.
	//
	// "First chunk" = the first event whose payload parses to a
	// choices[].delta. If no such chunk exists (empty / malformed
	// stream), we fall back to a single synthetic role+content chunk
	// at the top so the notice still surfaces.
	events, err := parseSSEEvents(body)
	if err != nil {
		// Parser failure — fall back to the synthetic-notice path so
		// the user still sees the notice. The original body would be
		// malformed anyway.
		return synthLeadingNoticeChatSSE(body, text), nil
	}
	var out bytes.Buffer
	injected := false
	for _, ev := range events {
		payload := ev.Data
		if !injected && payload != "" && payload != "[DONE]" {
			if rewritten, ok := mergeOpenAIChatChunkWithNotice([]byte(payload), text); ok {
				out.WriteString("data: ")
				out.Write(rewritten)
				out.WriteString("\n\n")
				injected = true
				continue
			}
		}
		out.WriteString("data: ")
		out.WriteString(payload)
		out.WriteString("\n\n")
	}
	// Restore the trailing [DONE] sentinel; parseSSEEvents filters
	// it out of the event list, but the harness needs to see it to
	// know the stream has ended cleanly.
	if bytes.Contains(body, []byte("[DONE]")) {
		out.WriteString("data: [DONE]\n\n")
	}
	if !injected {
		// No suitable chunk found — fall back to a leading synthetic
		// role+content pair so the user at least sees the notice.
		return synthLeadingNoticeChatSSE(out.Bytes(), text), nil
	}
	return out.Bytes(), nil
}

// synthLeadingNoticeChatSSE prepends a single synthetic chat-completion
// chunk carrying role:"assistant" + content:<text> to the supplied
// body. Used as the fallback when no upstream chunk is mergeable
// (empty / malformed stream); ensures the notice still surfaces even
// when the smart-merge path can't run.
func synthLeadingNoticeChatSSE(body []byte, text string) []byte {
	var prefix bytes.Buffer
	prefix.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":      "chatcmpl_clawvisor_notice",
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": text}, "finish_reason": nil}},
	}))
	prefix.Write(body)
	return prefix.Bytes()
}

// mergeOpenAIChatChunkWithNotice rewrites a single chat.completion.chunk
// payload to carry the notice text as its content field. The original
// choices[0].delta is preserved (role / tool_calls / etc.) and a
// `content` field is added or prefixed onto any existing content.
// Returns (out, true) when the chunk shape was understood and rewritten;
// (nil, false) when it didn't match the choices/delta shape and the
// caller should pass the original through unchanged and try the next
// line instead.
func mergeOpenAIChatChunkWithNotice(payload []byte, text string) ([]byte, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return nil, false
	}
	choicesRaw, ok := top["choices"]
	if !ok {
		return nil, false
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(choicesRaw, &choices); err != nil || len(choices) == 0 {
		return nil, false
	}
	deltaRaw, ok := choices[0]["delta"]
	if !ok {
		return nil, false
	}
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(deltaRaw, &delta); err != nil {
		return nil, false
	}
	// Combine notice with any pre-existing content on the delta.
	// content can be (a) absent / null, (b) a string, or (c) a
	// content-parts array (multimodal/vision deltas). Collapsing the
	// array case to a single string loses any image/audio parts the
	// upstream emitted, so we have to detect each shape.
	existingRaw, hasContent := delta["content"]
	hasContent = hasContent && len(existingRaw) > 0 && string(existingRaw) != "null"
	switch {
	case !hasContent:
		raw, err := json.Marshal(text)
		if err != nil {
			return nil, false
		}
		delta["content"] = raw
	case existingRaw[0] == '"':
		// String form.
		var existing string
		if err := json.Unmarshal(existingRaw, &existing); err != nil {
			return nil, false
		}
		combined := text
		if existing != "" {
			combined = text + "\n\n" + existing
		}
		raw, err := json.Marshal(combined)
		if err != nil {
			return nil, false
		}
		delta["content"] = raw
	case existingRaw[0] == '[':
		// Content-parts array form. Prepend a text part so the
		// existing multimodal payload (image_url, audio, …) survives.
		var parts []json.RawMessage
		if err := json.Unmarshal(existingRaw, &parts); err != nil {
			return nil, false
		}
		textPart, err := json.Marshal(map[string]any{"type": "text", "text": text})
		if err != nil {
			return nil, false
		}
		merged := append([]json.RawMessage{json.RawMessage(textPart)}, parts...)
		raw, err := json.Marshal(merged)
		if err != nil {
			return nil, false
		}
		delta["content"] = raw
	default:
		// Unknown shape — refuse rather than corrupt.
		return nil, false
	}
	deltaMarshaled, err := json.Marshal(delta)
	if err != nil {
		return nil, false
	}
	choices[0]["delta"] = deltaMarshaled
	choicesMarshaled, err := json.Marshal(choices)
	if err != nil {
		return nil, false
	}
	top["choices"] = choicesMarshaled
	out, err := json.Marshal(top)
	if err != nil {
		return nil, false
	}
	return out, true
}

// PrependOpenAIResponsesAssistantText inserts a leading
// message-with-output_text item into an OpenAI Responses-API
// response. JSON: prepends to output[]. SSE: emits
// response.output_item.added + response.output_text.delta +
// response.output_item.done events for the notice, then shifts the
// output_index on every subsequent event by +1.
func PrependOpenAIResponsesAssistantText(contentType string, body []byte, text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return body, nil
	}
	if isSSE(contentType) || looksLikeSSE(body) {
		return prependOpenAIResponsesAssistantTextSSE(body, text)
	}
	return prependOpenAIResponsesAssistantTextJSON(body, text)
}

func prependOpenAIResponsesAssistantTextJSON(body []byte, text string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, nil
	}
	outputRaw, ok := top["output"]
	if !ok {
		return body, nil
	}
	var output []json.RawMessage
	if err := json.Unmarshal(outputRaw, &output); err != nil {
		return body, nil
	}
	notice, err := json.Marshal(map[string]any{
		"type":   "message",
		"id":     "msg_clawvisor_notice",
		"role":   "assistant",
		"status": "completed",
		"content": []map[string]any{
			{"type": "output_text", "text": text},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("prepend openai responses: marshal notice item: %w", err)
	}
	merged := append([]json.RawMessage{json.RawMessage(notice)}, output...)
	mergedRaw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("prepend openai responses: marshal output: %w", err)
	}
	top["output"] = mergedRaw
	// `output_text` is the top-level convenience aggregation some
	// callers read. Keep it consistent by prefixing our notice — without
	// this, output_text drifts from output[] after a prepend.
	if otRaw, ok := top["output_text"]; ok && len(otRaw) > 0 {
		var existing string
		if err := json.Unmarshal(otRaw, &existing); err == nil {
			combined, err := json.Marshal(text + "\n\n" + existing)
			if err == nil {
				top["output_text"] = combined
			}
		}
	}
	out, err := json.Marshal(top)
	if err != nil {
		return nil, fmt.Errorf("prepend openai responses: marshal envelope: %w", err)
	}
	return out, nil
}

func prependOpenAIResponsesAssistantTextSSE(body []byte, text string) ([]byte, error) {
	events, err := parseSSEEvents(body)
	if err != nil {
		return body, nil
	}
	var out bytes.Buffer
	emit := func(name string, data any) error {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		out.WriteString("event: ")
		out.WriteString(name)
		out.WriteString("\ndata: ")
		out.Write(raw)
		out.WriteString("\n\n")
		return nil
	}

	noticeInserted := false
	// Inject the notice block immediately AFTER response.created (or
	// at the top if the stream skips response.created), then shift
	// every existing output_index by +1.
	//
	// The full six-event envelope (output_item.added →
	// content_part.added → output_text.delta → output_text.done →
	// content_part.done → output_item.done) mirrors what
	// synthOpenAIResponsesTextSSE emits. Codex CLI's renderer keys
	// off content_part.added + output_text.done specifically to
	// open and close a visible text block; without them the text
	// arrives but is never rendered. Every text-bearing event
	// must carry both item_id and content_index — the renderer
	// uses (item_id, content_index) as the part key and silently
	// drops events that don't address an open part.
	const noticeItemID = "msg_clawvisor_notice"
	insertNotice := func() error {
		if err := emit("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item": map[string]any{
				"type":   "message",
				"id":     noticeItemID,
				"role":   "assistant",
				"status": "in_progress",
			},
		}); err != nil {
			return err
		}
		if err := emit("response.content_part.added", map[string]any{
			"type":          "response.content_part.added",
			"item_id":       noticeItemID,
			"output_index":  0,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": ""},
		}); err != nil {
			return err
		}
		if err := emit("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       noticeItemID,
			"output_index":  0,
			"content_index": 0,
			"delta":         text,
		}); err != nil {
			return err
		}
		if err := emit("response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       noticeItemID,
			"output_index":  0,
			"content_index": 0,
			"text":          text,
		}); err != nil {
			return err
		}
		if err := emit("response.content_part.done", map[string]any{
			"type":          "response.content_part.done",
			"item_id":       noticeItemID,
			"output_index":  0,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": text},
		}); err != nil {
			return err
		}
		if err := emit("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": 0,
			"item": map[string]any{
				"type":   "message",
				"id":     noticeItemID,
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]any{
					{"type": "output_text", "text": text},
				},
			},
		}); err != nil {
			return err
		}
		noticeInserted = true
		return nil
	}

	for _, ev := range events {
		if ev.Event == "response.created" {
			// Pass through.
			out.WriteString("event: ")
			out.WriteString(ev.Event)
			out.WriteString("\ndata: ")
			out.WriteString(ev.Data)
			out.WriteString("\n\n")
			if !noticeInserted {
				if err := insertNotice(); err != nil {
					return body, nil
				}
			}
			continue
		}
		if !noticeInserted {
			if err := insertNotice(); err != nil {
				return body, nil
			}
		}
		// response.completed carries the final reconciled
		// `response.output[]` array. Clients that read from this
		// event (rather than reconstructing from per-item events)
		// would otherwise see a final response that omits the
		// notice item we emitted earlier in the stream. Rewrite
		// the completed envelope to prepend the notice message to
		// response.output[] in the same shape `insertNotice`
		// emitted on the per-item events.
		if ev.Event == "response.completed" {
			rewritten, ok := injectNoticeIntoResponsesCompleted(ev.Data, text)
			if ok {
				out.WriteString("event: ")
				out.WriteString(ev.Event)
				out.WriteString("\ndata: ")
				out.Write(rewritten)
				out.WriteString("\n\n")
				continue
			}
			// Couldn't parse — pass through unchanged. The per-item
			// notice events still surfaced earlier in the stream.
			out.WriteString("event: ")
			out.WriteString(ev.Event)
			out.WriteString("\ndata: ")
			out.WriteString(ev.Data)
			out.WriteString("\n\n")
			continue
		}
		// Shift output_index on any event that carries one.
		shifted, ok := shiftOpenAIResponsesEventIndex(ev.Data, 1)
		if !ok {
			out.WriteString("event: ")
			out.WriteString(ev.Event)
			out.WriteString("\ndata: ")
			out.WriteString(ev.Data)
			out.WriteString("\n\n")
			continue
		}
		out.WriteString("event: ")
		out.WriteString(ev.Event)
		out.WriteString("\ndata: ")
		out.Write(shifted)
		out.WriteString("\n\n")
	}
	if !noticeInserted {
		// Stream was empty or only carried events we don't model. As
		// a last resort, emit the notice item alone — that's still
		// useful information to the harness.
		_ = insertNotice()
	}
	return out.Bytes(), nil
}

// injectNoticeIntoResponsesCompleted rewrites a
// response.completed event payload to prepend a notice message item
// at response.output[0] AND prefix the notice text onto the top-level
// response.output_text aggregator if present. Mirrors the per-item
// notice events emitted earlier in the stream so clients that
// reconcile from response.completed see a consistent final state.
//
// Returns (nil, false) when the event shape isn't what we expect
// (no response field, output not an array, etc.) — caller passes
// the original through unchanged.
func injectNoticeIntoResponsesCompleted(data string, text string) ([]byte, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &top); err != nil {
		return nil, false
	}
	respRaw, ok := top["response"]
	if !ok {
		return nil, false
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(respRaw, &resp); err != nil {
		return nil, false
	}
	noticeItem, err := json.Marshal(map[string]any{
		"type":   "message",
		"id":     "msg_clawvisor_notice",
		"role":   "assistant",
		"status": "completed",
		"content": []map[string]any{
			{"type": "output_text", "text": text},
		},
	})
	if err != nil {
		return nil, false
	}
	// Prepend notice to response.output (create the array if absent
	// — covers a malformed completed event missing output but with
	// the rest of the envelope intact).
	var outputItems []json.RawMessage
	if existingRaw, hasOutput := resp["output"]; hasOutput && len(existingRaw) > 0 && string(existingRaw) != "null" {
		if err := json.Unmarshal(existingRaw, &outputItems); err != nil {
			return nil, false
		}
	}
	merged := append([]json.RawMessage{json.RawMessage(noticeItem)}, outputItems...)
	mergedRaw, err := json.Marshal(merged)
	if err != nil {
		return nil, false
	}
	resp["output"] = mergedRaw
	// Prefix the notice to the top-level output_text convenience
	// aggregator so clients reading that field see consistent
	// content. Skip silently if the field is absent or non-string.
	if otRaw, ok := resp["output_text"]; ok && len(otRaw) > 0 && otRaw[0] == '"' {
		var existing string
		if err := json.Unmarshal(otRaw, &existing); err == nil {
			combined := text
			if existing != "" {
				combined = text + "\n\n" + existing
			}
			if newRaw, err := json.Marshal(combined); err == nil {
				resp["output_text"] = newRaw
			}
		}
	}
	respMarshaled, err := json.Marshal(resp)
	if err != nil {
		return nil, false
	}
	top["response"] = respMarshaled
	out, err := json.Marshal(top)
	if err != nil {
		return nil, false
	}
	return out, true
}

// shiftOpenAIResponsesEventIndex bumps the `output_index` field of
// a Responses-API SSE event payload by delta. Returns (nil, false)
// when the event doesn't carry an output_index (e.g. response.created,
// response.completed) and the caller passes the original through
// unchanged.
func shiftOpenAIResponsesEventIndex(data string, delta int) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return nil, false
	}
	idxRaw, ok := obj["output_index"]
	if !ok {
		return nil, false
	}
	var idx int
	if err := json.Unmarshal(idxRaw, &idx); err != nil {
		return nil, false
	}
	idx += delta
	newIdx, err := json.Marshal(idx)
	if err != nil {
		return nil, false
	}
	obj["output_index"] = newIdx
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// shiftAnthropicEventIndex returns a copy of data with the top-level
// integer `index` field bumped by delta. Every other byte —
// including key order, whitespace, and any fields we don't model
// (estimated_tokens, future additions) — is preserved verbatim.
//
// Byte fidelity matters because Anthropic enforces a cryptographic
// signature over thinking blocks across turns: any reordering or
// stripping in the assistant turn causes "thinking blocks cannot be
// modified" 400s on the next request that includes that turn.
//
// Returns (nil, false) if the data isn't a JSON object with an integer
// top-level `index` — the caller passes the original event through
// unchanged in that case.
func shiftAnthropicEventIndex(event, data string, delta int) ([]byte, bool) {
	if delta == 0 {
		return []byte(data), true
	}
	valueStart, valueEnd, current, ok := findTopLevelIndexBytes(data)
	if !ok {
		return nil, false
	}
	b := []byte(data)
	// Drop the explicit capacity hint — CodeQL flags `len()+N`
	// arithmetic as potential overflow; bounded inputs make it
	// unreachable in practice but the analyzer can't see that.
	var out []byte
	out = append(out, b[:valueStart]...)
	out = strconv.AppendInt(out, int64(current+delta), 10)
	out = append(out, b[valueEnd:]...)
	return out, true
}

// setAnthropicEventIndex returns a copy of data with the top-level
// integer `index` field replaced by newIndex. Byte fidelity is
// preserved for all other content. See [shiftAnthropicEventIndex] for
// rationale.
func setAnthropicEventIndex(data string, newIndex int) ([]byte, bool) {
	valueStart, valueEnd, current, ok := findTopLevelIndexBytes(data)
	if !ok {
		return nil, false
	}
	if current == newIndex {
		return []byte(data), true
	}
	b := []byte(data)
	out := make([]byte, 0, len(b)+8)
	out = append(out, b[:valueStart]...)
	out = strconv.AppendInt(out, int64(newIndex), 10)
	out = append(out, b[valueEnd:]...)
	return out, true
}

// findTopLevelIndexBytes locates the byte range of the integer value
// of the top-level `index` field in a JSON object. Returns
// (valueStart, valueEnd, currentValue, true) where data[valueStart:valueEnd]
// is the encoded integer, or (_, _, _, false) on parse failure / missing
// field / non-integer value. Uses json.Decoder so nested "index"
// fields inside e.g. a tool_use's input object are not misidentified.
func findTopLevelIndexBytes(data string) (int, int, int, bool) {
	dec := json.NewDecoder(strings.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, 0, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, 0, false
	}
	b := []byte(data)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, 0, 0, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return 0, 0, 0, false
		}
		if key != "index" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, 0, false
			}
			continue
		}
		// dec.InputOffset() now sits just past the closing quote of
		// the "index" key. Scan forward past ':' and whitespace to
		// find the start of the value.
		p := int(dec.InputOffset())
		for p < len(b) && b[p] != ':' {
			p++
		}
		if p >= len(b) {
			return 0, 0, 0, false
		}
		p++ // past ':'
		for p < len(b) && (b[p] == ' ' || b[p] == '\t' || b[p] == '\n' || b[p] == '\r') {
			p++
		}
		valueStart := p
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, 0, false
		}
		valueEnd := int(dec.InputOffset())
		var idx int
		if err := json.Unmarshal(raw, &idx); err != nil {
			return 0, 0, 0, false
		}
		return valueStart, valueEnd, idx, true
	}
	return 0, 0, 0, false
}

func isFirstBlockThinking(events []sseEvent) bool {
	for _, ev := range events {
		if ev.Event == "content_block_start" {
			var cbs struct {
				ContentBlock struct {
					Type string `json:"type"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &cbs); err == nil {
				return cbs.ContentBlock.Type == "thinking" || cbs.ContentBlock.Type == "redacted_thinking"
			}
			break
		}
	}
	return false
}
