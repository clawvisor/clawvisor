package stream

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/clawvisor/clawvisor/internal/runtime/jsonpatch"
)

// AnthropicEncoder writes a sequence of canonical Events to an SSE
// destination using Anthropic Messages framing. Implements the
// three-state contract: pass-through events emit RawBytes verbatim
// (byte-faithful — thinking signatures survive); patched events apply
// FieldPatches to RawBytes via json_surgery; replaced events
// re-serialize from Parsed.
type AnthropicEncoder struct {
	w io.Writer
}

// NewAnthropicEncoder wraps w. The caller is responsible for flushing
// (via http.Flusher) after each Encode call when streaming to a client.
func NewAnthropicEncoder(w io.Writer) *AnthropicEncoder {
	return &AnthropicEncoder{w: w}
}

// Encode writes one Event to the destination per its state.
func (e *AnthropicEncoder) Encode(ev Event) error {
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("anthropic encoder: %w", err)
	}

	switch ev.State() {
	case StatePassThrough:
		// Pass-through: emit raw bytes verbatim. This is the path
		// thinking blocks travel through; bytes must not change.
		_, err := e.w.Write(ev.RawBytes)
		return err

	case StatePatched:
		// Patched: apply each FieldPatch to RawBytes via byte-faithful
		// surgery, then emit. Other bytes survive unchanged.
		patched, err := applyAnthropicPatches(ev)
		if err != nil {
			return fmt.Errorf("anthropic encoder: apply patches: %w", err)
		}
		_, err = e.w.Write(patched)
		return err

	case StateReplaced:
		// Replaced: serialize from Parsed. Used for events the policy
		// has genuinely rewritten.
		return e.emitReplaced(ev)
	}

	return fmt.Errorf("anthropic encoder: unreachable state for kind=%v", ev.Kind)
}

// applyAnthropicPatches walks the RawBytes of an SSE event (event:
// line + one or more data: lines + blank-line terminator) and applies
// each JSONPath patch by editing only the matching field in the data
// payload. All other bytes — including the SSE framing lines and any
// unmodified JSON fields — survive unchanged.
func applyAnthropicPatches(ev Event) ([]byte, error) {
	// Find the data: line(s). For Anthropic, one data: per event with
	// the entire JSON payload on one line.
	out := make([]byte, 0, len(ev.RawBytes))
	i := 0
	for i < len(ev.RawBytes) {
		// Locate the next newline (or end-of-buffer).
		j := i
		for j < len(ev.RawBytes) && ev.RawBytes[j] != '\n' {
			j++
		}
		line := ev.RawBytes[i:j]
		// Include the newline if present.
		nextI := j + 1
		if nextI > len(ev.RawBytes) {
			nextI = len(ev.RawBytes)
		}

		if len(line) > 5 && string(line[:5]) == "data:" {
			// Find the JSON payload's start position (skip "data:" and
			// any leading whitespace).
			payloadStart := 5
			for payloadStart < len(line) && (line[payloadStart] == ' ' || line[payloadStart] == '\t') {
				payloadStart++
			}
			payload := append([]byte(nil), line[payloadStart:]...)
			for _, patch := range ev.FieldPatches {
				patched, err := applyJSONPathPatch(payload, patch)
				if err != nil {
					return nil, fmt.Errorf("patch %q: %w", patch.JSONPath, err)
				}
				payload = patched
			}
			out = append(out, line[:payloadStart]...)
			out = append(out, payload...)
			if j < len(ev.RawBytes) {
				out = append(out, '\n')
			}
		} else {
			out = append(out, line...)
			if j < len(ev.RawBytes) {
				out = append(out, '\n')
			}
		}

		i = nextI
	}
	return out, nil
}

// applyJSONPathPatch applies a single FieldPatch. Only top-level
// fields are supported (no dots in JSONPath) — that covers the common
// case of `index` shifts. Nested patches (e.g., `delta.text`) should
// be added when a policy needs them.
func applyJSONPathPatch(data []byte, patch FieldPatch) ([]byte, error) {
	if patch.JSONPath == "" {
		return nil, fmt.Errorf("empty JSONPath")
	}
	// Only top-level field support for now; refuse anything dotted so
	// we don't silently swallow a malformed path.
	for _, c := range patch.JSONPath {
		if c == '.' {
			return nil, fmt.Errorf("nested JSONPath %q not yet supported", patch.JSONPath)
		}
	}
	return jsonpatch.SetTopLevelField(data, patch.JSONPath, patch.NewValue)
}

// emitReplaced serializes a REPLACED-state event from its Parsed
// payload. Used when a policy fully rewrites an event's content.
func (e *AnthropicEncoder) emitReplaced(ev Event) error {
	if ev.Meta.SSEEventName == "" {
		return fmt.Errorf("anthropic encoder: replaced event has no SSEEventName in Meta")
	}

	// Build the data payload from the typed Parsed value. The shape
	// depends on Kind + the payload type. Only a few combinations are
	// supported today — additional ones land migration-by-migration.
	payload, err := serializeAnthropicPayload(ev)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(e.w, "event: %s\n", ev.Meta.SSEEventName); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(e.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	return nil
}

// serializeAnthropicPayload builds the JSON data line for a replaced
// event. The shape mirrors what Anthropic emits, indexed by
// (EventKind, payload type).
func serializeAnthropicPayload(ev Event) ([]byte, error) {
	switch p := ev.Parsed.(type) {
	case TextBlock:
		idx := 0
		if ev.Meta.AnthropicIndex >= 0 {
			idx = ev.Meta.AnthropicIndex
		}
		switch ev.Kind {
		case KindBlockStart:
			return json.Marshal(map[string]any{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		case KindBlockDelta:
			return json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]any{"type": "text_delta", "text": p.Text},
			})
		case KindBlockEnd:
			return json.Marshal(map[string]any{
				"type":  "content_block_stop",
				"index": idx,
			})
		}
	}
	return nil, fmt.Errorf("anthropic encoder: serializeAnthropicPayload: unsupported (kind=%v, type=%T)", ev.Kind, ev.Parsed)
}
