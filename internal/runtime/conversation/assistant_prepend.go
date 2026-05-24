package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	merged := make([]json.RawMessage, 0, len(content)+1)
	merged = append(merged, json.RawMessage(textBlock))
	merged = append(merged, content...)
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

	textBlockInserted := false
	for _, ev := range events {
		switch ev.Event {
		case "message_start":
			// Pass through verbatim.
			out.WriteString("event: ")
			out.WriteString(ev.Event)
			out.WriteString("\ndata: ")
			out.WriteString(ev.Data)
			out.WriteString("\n\n")
			// Inject our text block immediately after message_start so
			// the harness renders the notice before any tool_use the
			// upstream emitted.
			if err := emit("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": 0,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			}); err != nil {
				return body, nil
			}
			if err := emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": text},
			}); err != nil {
				return body, nil
			}
			if err := emit("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": 0,
			}); err != nil {
				return body, nil
			}
			textBlockInserted = true
		case "content_block_start", "content_block_delta", "content_block_stop":
			if !textBlockInserted {
				// No message_start observed yet (malformed stream). Pass
				// through unchanged; we'd rather render a broken
				// notice than corrupt the original event order.
				out.WriteString("event: ")
				out.WriteString(ev.Event)
				out.WriteString("\ndata: ")
				out.WriteString(ev.Data)
				out.WriteString("\n\n")
				continue
			}
			shifted, ok := shiftAnthropicEventIndex(ev.Event, ev.Data, 1)
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
		default:
			out.WriteString("event: ")
			out.WriteString(ev.Event)
			out.WriteString("\ndata: ")
			out.WriteString(ev.Data)
			out.WriteString("\n\n")
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
	// Walk lines to find the first `data:` event so we can borrow its
	// `id` for our injected chunks. Chat Completions SSE doesn't use
	// `event:` headers — every payload is `data: {...}`.
	lines := strings.Split(string(body), "\n")
	var firstID string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			continue
		}
		firstID = probe.ID
		break
	}
	if firstID == "" {
		firstID = "chatcmpl_clawvisor_notice"
	}

	var out bytes.Buffer
	// Inject two leading chunks: one carrying role:"assistant" (some
	// harnesses key off the role-only chunk to open the assistant
	// turn), one carrying our notice content. The upstream's
	// subsequent chunks (which themselves set role:"assistant") then
	// stream in — duplicate role assignment is a no-op for harnesses
	// in practice.
	out.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":      firstID,
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	}))
	out.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":      firstID,
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
	}))
	// Pass through the original body verbatim. Adding ours at the top
	// preserves all upstream events, including ones we don't model.
	out.Write(body)
	return out.Bytes(), nil
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
	insertNotice := func() error {
		if err := emit("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item": map[string]any{
				"type":    "message",
				"id":      "msg_clawvisor_notice",
				"role":    "assistant",
				"status":  "in_progress",
				"content": []any{},
			},
		}); err != nil {
			return err
		}
		if err := emit("response.output_text.delta", map[string]any{
			"type":         "response.output_text.delta",
			"output_index": 0,
			"delta":        text,
		}); err != nil {
			return err
		}
		if err := emit("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": 0,
			"item": map[string]any{
				"type":   "message",
				"id":     "msg_clawvisor_notice",
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

// shiftAnthropicEventIndex re-serialises a content_block_* event with
// its `index` field bumped by delta. The event data is preserved
// byte-for-byte except for the index field. Returns (nil, false) if
// the data isn't a JSON object with an integer index — the caller
// passes the original event through unchanged in that case.
func shiftAnthropicEventIndex(event, data string, delta int) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return nil, false
	}
	idxRaw, ok := obj["index"]
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
	obj["index"] = newIdx
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}
