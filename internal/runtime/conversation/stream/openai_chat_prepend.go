package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// PrependOpenAIChatAssistantNotice consumes an OpenAI Chat
// Completions SSE stream from src and writes a transformed stream
// to dst that surfaces the notice text to the harness.
//
// Strategy: emit a synthetic leading chat.completion.chunk carrying
// content:<notice> + finish_reason:null, then
// pass through every upstream chunk verbatim. This matches the
// fallback path in the legacy streaming_assistant_prepend writer
// (`emitSyntheticChatNotice` followed by upstream passthrough).
//
// The legacy writer's primary path *merges* the notice into the
// first upstream chunk's delta to avoid emitting a second
// role:"assistant" header (which strict accumulators interpret as a
// new assistant turn). The synthetic-leading-chunk path intentionally
// omits delta.role so the upstream's real first role chunk remains the
// single assistant-role marker on the wire.
//
// Blank notice copies the stream verbatim (no-op).
func PrependOpenAIChatAssistantNotice(dst io.Writer, src io.Reader, notice string) error {
	if notice == "" {
		_, err := io.Copy(dst, src)
		return err
	}

	// Emit the synthetic leading chunk first.
	if err := writeOpenAIChatNoticeChunk(dst, notice); err != nil {
		return err
	}

	// Pass through every upstream event verbatim.
	d := NewOpenAIChatDecoder(src)
	e := NewOpenAIChatEncoder(dst)
	sawDone := false
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			if !sawDone {
				_, err := io.WriteString(dst, "data: [DONE]\n\n")
				return err
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("openai chat prepend: decode: %w", err)
		}
		if ev.Kind == KindResponseEnd {
			sawDone = true
		}
		if err := e.Encode(ev); err != nil {
			return fmt.Errorf("openai chat prepend: encode: %w", err)
		}
	}
}

// writeOpenAIChatNoticeChunk emits the synthetic leading SSE event
// carrying the notice. Shape matches what OpenAI's `/v1/chat/completions`
// stream emits for the first text chunk.
func writeOpenAIChatNoticeChunk(dst io.Writer, notice string) error {
	chunk := map[string]any{
		"id":     "chatcmpl_clawvisor_notice",
		"object": "chat.completion.chunk",
		// Use a deterministic positive timestamp so strict clients see
		// a valid created field without making tests time-dependent.
		"created": int64(1),
		"model":   "clawvisor-notice",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{"content": notice},
			"finish_reason": nil,
		}},
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(dst, "data: %s\n\n", raw); err != nil {
		return err
	}
	return nil
}
