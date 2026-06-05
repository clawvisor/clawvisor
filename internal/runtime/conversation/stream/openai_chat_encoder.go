package stream

import (
	"fmt"
	"io"
)

// OpenAIChatEncoder writes canonical Events back to OpenAI Chat
// Completions SSE framing. The three-state contract applies identically
// to Anthropic — only the wire framing differs.
type OpenAIChatEncoder struct {
	w io.Writer
}

func NewOpenAIChatEncoder(w io.Writer) *OpenAIChatEncoder {
	return &OpenAIChatEncoder{w: w}
}

func (e *OpenAIChatEncoder) Encode(ev Event) error {
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("openai chat encoder: %w", err)
	}

	switch ev.State() {
	case StatePassThrough:
		_, err := e.w.Write(ev.RawBytes)
		return err

	case StatePatched:
		// Patched path uses the same JSON-surgery edit Anthropic uses;
		// the patches are applied to the `data:` line's JSON payload
		// while the framing bytes survive.
		patched, err := applyAnthropicPatches(ev) // same shape: data: <json> + newlines
		if err != nil {
			return fmt.Errorf("openai chat encoder: apply patches: %w", err)
		}
		_, err = e.w.Write(patched)
		return err

	case StateReplaced:
		// REPLACED events for OpenAI Chat are uncommon — the proxy
		// historically merges notice into the first upstream chunk
		// rather than emitting a new event. Supported here for
		// completeness; the format is `data: <json>\n\n`.
		return fmt.Errorf("openai chat encoder: REPLACED state not yet supported (use PATCHED or pass-through)")
	}

	return fmt.Errorf("openai chat encoder: unreachable state for kind=%v", ev.Kind)
}
