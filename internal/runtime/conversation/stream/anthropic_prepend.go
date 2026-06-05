package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// PrependAnthropicAssistantNotice consumes an Anthropic SSE stream from
// src and writes a transformed stream to dst that has a leading
// assistant text block carrying the given notice. Subsequent block
// indices are shifted by +1 via FieldPatch — sibling bytes remain
// untouched.
//
// This is the Phase 2 proof-of-concept: it reproduces (a subset of)
// the behavior in conversation/streaming_assistant_prepend.go through
// the new event-stream model. The reference implementation handles
// edge cases (thinking-block deferral, stream-end fallback, partial
// events) we'll port in follow-ups; this version pins the happy path
// to validate the contract.
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
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("prepend notice: decode: %w", err)
		}

		// Inject the notice block right after message_start.
		// We emit three REPLACED events (start/delta/stop) at index 0.
		if ev.Kind == KindResponseStart && !injected {
			if err := e.Encode(ev); err != nil {
				return err
			}
			if err := writeAnthropicNoticeBlock(e, notice); err != nil {
				return err
			}
			injected = true
			continue
		}

		// Once the notice is injected, shift any content_block_* event
		// index by +1 so upstream blocks slot in after the notice.
		if injected && hasAnthropicIndex(ev.Kind) && ev.Meta.AnthropicIndex >= 0 {
			shifted := ev.Meta.AnthropicIndex + 1
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
	return nil
}

// writeAnthropicNoticeBlock emits the three SSE events that compose a
// new text block at index 0 carrying the notice. Each event is in the
// REPLACED state (Parsed populated, RawBytes empty) so the encoder
// serializes from the typed payload.
func writeAnthropicNoticeBlock(e *AnthropicEncoder, notice string) error {
	events := []Event{
		{
			Kind:   KindBlockStart,
			Meta:   EventMeta{SSEEventName: "content_block_start", AnthropicIndex: 0},
			Parsed: TextBlock{},
		},
		{
			Kind:   KindBlockDelta,
			Meta:   EventMeta{SSEEventName: "content_block_delta", AnthropicIndex: 0},
			Parsed: TextBlock{Text: notice},
		},
		{
			Kind:   KindBlockEnd,
			Meta:   EventMeta{SSEEventName: "content_block_stop", AnthropicIndex: 0},
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
