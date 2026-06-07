package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// PrependAnthropicAssistantNotice consumes an Anthropic SSE stream from
// src and writes a transformed stream to dst that has a leading
// assistant text block carrying the given notice. Subsequent block
// indices are shifted by +1 via FieldPatch — sibling bytes remain
// untouched.
//
// This helper covers the common notice-prepend path. The reference
// implementation in conversation/streaming_assistant_prepend.go handles
// additional edge cases such as thinking-block deferral, stream-end
// fallback, and partial events.
//
// Returns an error from the underlying io operations. The notice text
// must be non-empty; blank text is a no-op (the stream is copied
// verbatim).
func PrependAnthropicAssistantNotice(dst io.Writer, src io.Reader, notice string) error {
	if notice == "" {
		_, err := io.Copy(dst, src)
		return err
	}

	d := NewAnthropicDecoder(src)
	e := NewAnthropicEncoder(dst)

	injected := false
	afterMessageStart := false
	inThinkingBlock := false
	noticeIndex := 0
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("prepend notice: decode: %w", err)
		}

		if ev.Kind == KindResponseStart && !afterMessageStart {
			if err := e.Encode(ev); err != nil {
				return err
			}
			afterMessageStart = true
			continue
		}

		if afterMessageStart && !injected {
			if ev.Kind == KindBlockStart && isAnthropicThinkingBlockStart(ev) {
				if ev.Meta.AnthropicIndex >= noticeIndex {
					noticeIndex = ev.Meta.AnthropicIndex + 1
				}
				inThinkingBlock = true
				if err := e.Encode(ev); err != nil {
					return err
				}
				continue
			}
			if inThinkingBlock {
				if err := e.Encode(ev); err != nil {
					return err
				}
				if ev.Kind == KindBlockEnd {
					inThinkingBlock = false
				}
				continue
			}
			if err := writeAnthropicNoticeBlock(e, notice, noticeIndex); err != nil {
				return err
			}
			injected = true
		}
		// Once the notice is injected, shift any content_block_* event
		// at or after the injected index by +1 so upstream blocks slot
		// in after the notice while leading thinking blocks keep their
		// original signed order.
		if injected && hasAnthropicIndex(ev.Kind) && (ev.Meta.AnthropicIndex < 0 || ev.Meta.AnthropicIndex >= noticeIndex) {
			shifted := ev.Meta.AnthropicIndex + 1
			if ev.Meta.AnthropicIndex < 0 {
				shifted = noticeIndex + 1
			}
			ev.FieldPatches = append(ev.FieldPatches, FieldPatch{
				JSONPath: "index",
				NewValue: json.RawMessage(fmt.Sprintf("%d", shifted)),
			})
			ev.Meta.AnthropicIndex = shifted
		}

		if err := e.Encode(ev); err != nil {
			return err
		}
	}
	if !injected {
		if err := writeAnthropicNoticeBlock(e, notice, noticeIndex); err != nil {
			return err
		}
	}
	return nil
}

// writeAnthropicNoticeBlock emits the three SSE events that compose a
// new text block at index 0 carrying the notice. Each event is in the
// REPLACED state (Parsed populated, RawBytes empty) so the encoder
// serializes from the typed payload.
func writeAnthropicNoticeBlock(e *AnthropicEncoder, notice string, index int) error {
	events := []Event{
		{
			Kind:   KindBlockStart,
			Meta:   EventMeta{SSEEventName: "content_block_start", AnthropicIndex: index},
			Parsed: TextBlock{},
		},
		{
			Kind:   KindBlockDelta,
			Meta:   EventMeta{SSEEventName: "content_block_delta", AnthropicIndex: index},
			Parsed: TextBlock{Text: notice},
		},
		{
			Kind:   KindBlockEnd,
			Meta:   EventMeta{SSEEventName: "content_block_stop", AnthropicIndex: index},
			Parsed: TextBlock{},
		},
	}
	for _, ev := range events {
		if err := e.Encode(ev); err != nil {
			return err
		}
	}
	return nil
}

func isAnthropicThinkingBlockStart(ev Event) bool {
	if ev.Kind != KindBlockStart || len(ev.RawBytes) == 0 {
		return false
	}
	data := sseDataPayload(ev.RawBytes)
	if data == "" {
		return false
	}
	var payload struct {
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return false
	}
	return payload.ContentBlock.Type == "thinking"
}

func sseDataPayload(raw []byte) string {
	var out strings.Builder
	lines := bytes.Split(raw, []byte{'\n'})
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			out.Write(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		}
	}
	return out.String()
}

// hasAnthropicIndex reports whether the event kind carries an `index`
// field that participates in shifting after a leading block is
// injected.
func hasAnthropicIndex(k EventKind) bool {
	switch k {
	case KindBlockStart, KindBlockDelta, KindBlockEnd:
		return true
	}
	return false
}
