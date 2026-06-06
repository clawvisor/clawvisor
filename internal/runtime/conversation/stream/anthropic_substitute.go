package stream

import (
	"encoding/json"
	"fmt"
	"io"
)

// SubstituteAnthropicResponse emits a synthetic Anthropic Messages SSE
// stream carrying a single text block with the provided text. The
// upstream src reader is drained-and-discarded — its content is
// thrown away, replaced entirely by the synthetic stream.
//
// Used when a policy substitutes the entire assistant response (e.g.,
// inline_task_intercept replaces the model's POST /api/control/tasks
// tool_use with a human-readable approval prompt).
//
// The emitted sequence mirrors what Anthropic emits for a one-text-block
// turn: message_start → content_block_start → content_block_delta →
// content_block_stop → message_delta → message_stop.
func SubstituteAnthropicResponse(dst io.Writer, src io.Reader, text string) error {
	// Drain src to keep the upstream connection clean (some pools track
	// drained vs cancelled). The content is discarded.
	if _, err := io.Copy(io.Discard, src); err != nil {
		// Drain failure doesn't fail the substitution; we still emit the
		// synthetic response. Substitution is best-effort after the
		// caller has decided to replace the upstream body.
	}

	events := []struct {
		name    string
		payload map[string]any
	}{
		{
			name: "message_start",
			payload: map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":            "msg_clawvisor_substitute",
					"type":          "message",
					"role":          "assistant",
					"model":         "claude-substituted",
					"content":       []any{},
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage": map[string]any{
						"input_tokens":  0,
						"output_tokens": 0,
					},
				},
			},
		},
		{
			name: "content_block_start",
			payload: map[string]any{
				"type":          "content_block_start",
				"index":         0,
				"content_block": map[string]any{"type": "text", "text": ""},
			},
		},
		{
			name: "content_block_delta",
			payload: map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": text},
			},
		},
		{
			name: "content_block_stop",
			payload: map[string]any{
				"type":  "content_block_stop",
				"index": 0,
			},
		},
		{
			name: "message_delta",
			payload: map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": 0},
			},
		},
		{
			name:    "message_stop",
			payload: map[string]any{"type": "message_stop"},
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
