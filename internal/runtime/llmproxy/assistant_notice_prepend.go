package llmproxy

import (
	"bytes"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// PrependAssistantNotice splices a user-facing notice text into the
// upstream response's assistant turn so the harness shows it inline
// alongside the model's reply. Returns (body, false, nil) when the
// notice is empty or the body shape is unrecognized; (body, true, nil)
// when the body was mutated.
func PrependAssistantNotice(
	provider conversation.Provider,
	contentType string,
	body []byte,
	text string,
) ([]byte, bool, error) {
	if strings.TrimSpace(text) == "" {
		return body, false, nil
	}
	out, err := dispatchPrependNotice(provider, contentType, body, text)
	if err != nil {
		return nil, false, err
	}
	if len(out) == 0 {
		return body, false, nil
	}
	// Use bytes.Equal rather than slice-identity comparison. The
	// per-provider helpers historically returned the ORIGINAL slice
	// untouched on no-op, but that contract is implicit and brittle —
	// a helper that ever returns a fresh slice (e.g., after a defensive
	// copy) would silently re-render the body as "mutated" and force
	// the Content-Length / Content-Encoding cleanup downstream to fire
	// for nothing. Byte-equality reads the actual semantic. The cost
	// is one O(n) scan over response bodies that are already small.
	if bytes.Equal(out, body) {
		return body, false, nil
	}
	return out, true, nil
}

func dispatchPrependNotice(
	provider conversation.Provider,
	contentType string,
	body []byte,
	text string,
) ([]byte, error) {
	shape := DefaultInboundShapeRegistry().ForProvider(provider)
	if shape == nil {
		return body, nil
	}
	return shape.PrependAssistantText(contentType, body, text)
}
