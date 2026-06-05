package stream

import (
	"fmt"
	"io"
)

// GoogleEncoder writes canonical Events back to Gemini SSE framing.
// Same shape as OpenAIChatEncoder: each event is one `data:` line
// followed by a blank line.
type GoogleEncoder struct {
	w io.Writer
}

func NewGoogleEncoder(w io.Writer) *GoogleEncoder {
	return &GoogleEncoder{w: w}
}

func (e *GoogleEncoder) Encode(ev Event) error {
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("google encoder: %w", err)
	}

	switch ev.State() {
	case StatePassThrough:
		_, err := e.w.Write(ev.RawBytes)
		return err

	case StatePatched:
		// PATCHED uses the same SSE framing surgery as Anthropic and
		// OpenAI Chat.
		patched, err := applyAnthropicPatches(ev)
		if err != nil {
			return fmt.Errorf("google encoder: apply patches: %w", err)
		}
		_, err = e.w.Write(patched)
		return err

	case StateReplaced:
		// REPLACED for Gemini isn't supported yet — no caller needs it
		// at the stub stage. Error loudly so misuse is visible.
		return fmt.Errorf("google encoder: REPLACED state not yet supported")
	}

	return fmt.Errorf("google encoder: unreachable state for kind=%v", ev.Kind)
}
