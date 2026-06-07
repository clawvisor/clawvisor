package stream

import (
	"bytes"
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
	if !injected {
		if err := writeOpenAIResponsesNoticeEnvelope(dst, notice); err != nil {
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
	noticeRaw, err := json.Marshal(openAIResponsesNoticeItem(notice))
	if err != nil {
		return nil, false, err
	}
	return rewriteFirstSSEDataPayload(raw, func(data []byte) ([]byte, bool, error) {
		return insertNoticeIntoOutputArray(data, noticeRaw)
	})
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

func rewriteFirstSSEDataPayload(raw []byte, rewrite func([]byte) ([]byte, bool, error)) ([]byte, bool, error) {
	lines := bytes.SplitAfter(raw, []byte{'\n'})
	offset := 0
	for _, line := range lines {
		trimmed := bytes.TrimRight(line, "\r\n")
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			offset += len(line)
			continue
		}
		valueStart := offset + len("data:")
		for valueStart < offset+len(trimmed) && (raw[valueStart] == ' ' || raw[valueStart] == '\t') {
			valueStart++
		}
		valueEnd := offset + len(trimmed)
		next, ok, err := rewrite(raw[valueStart:valueEnd])
		if err != nil || !ok {
			return nil, ok, err
		}
		out := make([]byte, 0, len(raw)+len(next)-(valueEnd-valueStart))
		out = append(out, raw[:valueStart]...)
		out = append(out, next...)
		out = append(out, raw[valueEnd:]...)
		return out, true, nil
	}
	return nil, false, nil
}

func insertNoticeIntoOutputArray(data []byte, noticeRaw []byte) ([]byte, bool, error) {
	key := []byte(`"output"`)
	keyPos := bytes.Index(data, key)
	if keyPos < 0 {
		return nil, false, nil
	}
	p := keyPos + len(key)
	for p < len(data) && data[p] != ':' {
		p++
	}
	if p >= len(data) {
		return nil, false, nil
	}
	p++
	for p < len(data) && (data[p] == ' ' || data[p] == '\t' || data[p] == '\n' || data[p] == '\r') {
		p++
	}
	if p >= len(data) || data[p] != '[' {
		return nil, false, nil
	}
	insertPos := p + 1
	needsComma := false
	for q := insertPos; q < len(data); q++ {
		switch data[q] {
		case ' ', '\t', '\n', '\r':
			continue
		case ']':
			needsComma = false
		default:
			needsComma = true
		}
		break
	}
	out := make([]byte, 0, len(data)+len(noticeRaw)+1)
	out = append(out, data[:insertPos]...)
	out = append(out, noticeRaw...)
	if needsComma {
		out = append(out, ',')
	}
	out = append(out, data[insertPos:]...)
	return out, true, nil
}
