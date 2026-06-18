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
	// The per-provider helpers return the ORIGINAL body slice
	// untouched when they decide not to mutate (shape unrecognized,
	// content field absent, …). Compare slice headers — same
	// backing array + same length means literally the same slice
	// returned, which is the no-op signal from the helper. This is
	// stricter than bytes.Equal (which would also flag a genuine
	// change as no-op if the new bytes happened to match) AND
	// independent of encoding/json's marshal stability.
	if len(out) == len(body) && (len(body) == 0 || &out[0] == &body[0]) {
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
	switch provider {
	case conversation.ProviderAnthropic:
		return conversation.PrependAnthropicAssistantText(contentType, body, text)
	case conversation.ProviderOpenAI:
		switch {
		case bytes.Contains(body, []byte(`"choices"`)) && !bytes.Contains(body, []byte(`response.output_item`)):
			return conversation.PrependOpenAIChatAssistantText(contentType, body, text)
		case bytes.Contains(body, []byte(`"output"`)) || bytes.Contains(body, []byte(`response.output_item`)):
			return conversation.PrependOpenAIResponsesAssistantText(contentType, body, text)
		default:
			return body, nil
		}
	default:
		return body, nil
	}
}
