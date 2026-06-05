package stream

import (
	"fmt"
	"io"
)

// OpenAIResponsesEncoder writes canonical Events back to the OpenAI
// Responses SSE wire shape. Framing is identical to Anthropic
// (`event:`/`data:`/blank-line), so the byte-surgery for PATCHED
// events reuses applyAnthropicPatches verbatim.
type OpenAIResponsesEncoder struct {
	w io.Writer
}

func NewOpenAIResponsesEncoder(w io.Writer) *OpenAIResponsesEncoder {
	return &OpenAIResponsesEncoder{w: w}
}

func (e *OpenAIResponsesEncoder) Encode(ev Event) error {
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("openai responses encoder: %w", err)
	}

	switch ev.State() {
	case StatePassThrough:
		_, err := e.w.Write(ev.RawBytes)
		return err

	case StatePatched:
		patched, err := applyAnthropicPatches(ev) // shared SSE framing
		if err != nil {
			return fmt.Errorf("openai responses encoder: apply patches: %w", err)
		}
		_, err = e.w.Write(patched)
		return err

	case StateReplaced:
		// REPLACED events for OpenAI Responses arrive when a notice
		// envelope is being synthesized. The notice envelope is six
		// linked events; emitting one in isolation is unusual. Leave
		// REPLACED unsupported until a real caller arrives so any
		// misuse fails loudly.
		return fmt.Errorf("openai responses encoder: REPLACED state not yet supported (use PATCHED or pass-through)")
	}

	return fmt.Errorf("openai responses encoder: unreachable state for kind=%v", ev.Kind)
}
