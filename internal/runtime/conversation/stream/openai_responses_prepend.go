package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const openAIResponsesNoticeItemID = "msg_clawvisor_notice"

// PrependOpenAIResponsesAssistantNotice consumes an OpenAI Responses
// SSE stream from src, injects a six-event notice envelope at
// output_index 0 immediately after response.created, and shifts every
// subsequent event's output_index by +1 via FieldPatch (PATCHED
// state — sibling bytes survive).
//
// The notice envelope mirrors what the legacy
// streaming_assistant_prepend.emitOpenAIResponsesNotice writer emits:
// added → content_part.added → output_text.delta → output_text.done →
// content_part.done → output_item.done, all sharing item_id
// "msg_clawvisor_notice" at output_index 0.
func PrependOpenAIResponsesAssistantNotice(dst io.Writer, src io.Reader, notice string) error {
	if notice == "" {
		_, err := io.Copy(dst, src)
		return err
	}

	d := NewOpenAIResponsesDecoder(src)
	e := NewOpenAIResponsesEncoder(dst)

	injected := false
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("openai responses prepend: decode: %w", err)
		}

		if !injected && ev.Kind == KindResponseStart {
			if err := e.Encode(ev); err != nil {
				return err
			}
			if err := writeOpenAIResponsesNoticeEnvelope(dst, notice); err != nil {
				return err
			}
			injected = true
			continue
		}

		// After notice injection, every event carrying output_index
		// must shift by +1 to make room at index 0.
		if injected && ev.Meta.OpenAIOutputIndex >= 0 {
			shifted := ev.Meta.OpenAIOutputIndex + 1
			ev.FieldPatches = append(ev.FieldPatches, FieldPatch{
				JSONPath: "output_index",
				NewValue: json.RawMessage(fmt.Sprintf("%d", shifted)),
			})
			ev.Meta.OpenAIOutputIndex = shifted
		}
		if injected && ev.Meta.SSEEventName == "response.completed" {
			raw, ok, err := rewriteOpenAIResponsesCompleted(ev.RawBytes, notice)
			if err != nil {
				return err
			}
			if ok {
				ev.RawBytes = raw
				ev.FieldPatches = nil
			}
		}

		if err := e.Encode(ev); err != nil {
			return err
		}
	}
	return nil
}

// writeOpenAIResponsesNoticeEnvelope emits the six linked events that
// constitute the notice item at output_index 0. Each event is
// individually self-contained on the wire; together they describe a
// completed assistant message carrying the notice text.
func writeOpenAIResponsesNoticeEnvelope(dst io.Writer, notice string) error {
	events := []struct {
		name    string
		payload map[string]any
	}{
		{
			name: "response.output_item.added",
			payload: map[string]any{
				"type":         "response.output_item.added",
				"output_index": 0,
				"item": map[string]any{
					"type":   "message",
					"id":     openAIResponsesNoticeItemID,
					"role":   "assistant",
					"status": "in_progress",
				},
			},
		},
		{
			name: "response.content_part.added",
			payload: map[string]any{
				"type":          "response.content_part.added",
				"item_id":       openAIResponsesNoticeItemID,
				"output_index":  0,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": ""},
			},
		},
		{
			name: "response.output_text.delta",
			payload: map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       openAIResponsesNoticeItemID,
				"output_index":  0,
				"content_index": 0,
				"delta":         notice,
			},
		},
		{
			name: "response.output_text.done",
			payload: map[string]any{
				"type":          "response.output_text.done",
				"item_id":       openAIResponsesNoticeItemID,
				"output_index":  0,
				"content_index": 0,
				"text":          notice,
			},
		},
		{
			name: "response.content_part.done",
			payload: map[string]any{
				"type":          "response.content_part.done",
				"item_id":       openAIResponsesNoticeItemID,
				"output_index":  0,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": notice},
			},
		},
		{
			name: "response.output_item.done",
			payload: map[string]any{
				"type":         "response.output_item.done",
				"output_index": 0,
				"item": map[string]any{
					"type":   "message",
					"id":     openAIResponsesNoticeItemID,
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": notice},
					},
				},
			},
		},
	}

	for _, ev := range events {
		raw, err := json.Marshal(ev.payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(dst, "event: %s\ndata: %s\n\n", ev.name, raw); err != nil {
			return err
		}
	}
	return nil
}

func rewriteOpenAIResponsesCompleted(raw []byte, notice string) ([]byte, bool, error) {
	data := sseDataPayload(raw)
	if data == "" {
		return nil, false, nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return nil, false, err
	}
	response, ok := payload["response"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	output, _ := response["output"].([]any)
	response["output"] = append([]any{openAIResponsesNoticeItem(notice)}, output...)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	return []byte(fmt.Sprintf("event: response.completed\ndata: %s\n\n", encoded)), true, nil
}

func openAIResponsesNoticeItem(notice string) map[string]any {
	return map[string]any{
		"type":   "message",
		"id":     openAIResponsesNoticeItemID,
		"role":   "assistant",
		"status": "completed",
		"content": []map[string]any{
			{"type": "output_text", "text": notice},
		},
	}
}
