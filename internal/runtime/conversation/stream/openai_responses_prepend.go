package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

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
//
// Limitation: response.completed rewriting (to include the notice
// item in the final response.output array) isn't done yet. Strict
// reconcilers that read response.output may not see the notice item
// in the final state. Streaming consumers that watch the per-event
// deltas see the notice text correctly. A future rewrite should handle
// the embedded response object.
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
	const itemID = "msg_clawvisor_notice"

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
					"id":     itemID,
					"role":   "assistant",
					"status": "in_progress",
				},
			},
		},
		{
			name: "response.content_part.added",
			payload: map[string]any{
				"type":          "response.content_part.added",
				"item_id":       itemID,
				"output_index":  0,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": ""},
			},
		},
		{
			name: "response.output_text.delta",
			payload: map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       itemID,
				"output_index":  0,
				"content_index": 0,
				"delta":         notice,
			},
		},
		{
			name: "response.output_text.done",
			payload: map[string]any{
				"type":          "response.output_text.done",
				"item_id":       itemID,
				"output_index":  0,
				"content_index": 0,
				"text":          notice,
			},
		},
		{
			name: "response.content_part.done",
			payload: map[string]any{
				"type":          "response.content_part.done",
				"item_id":       itemID,
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
					"id":     itemID,
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
