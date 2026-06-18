package llmproxy

import (
	"encoding/json"
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
		switch openAIResponseShape(contentType, body) {
		case openAIResponseShapeChat:
			return conversation.PrependOpenAIChatAssistantText(contentType, body, text)
		case openAIResponseShapeResponses:
			return conversation.PrependOpenAIResponsesAssistantText(contentType, body, text)
		default:
			return body, nil
		}
	default:
		return body, nil
	}
}

// openAIResponseShape distinguishes Chat Completions (`choices[]`) from
// Responses API (`output[]`) bodies by parsing the JSON top level — or
// for SSE bodies, by sniffing the leading event marker. Substring
// search across the whole body is unreliable because both keys can
// appear in nested content; only the top-level / event-prefix presence
// is a stable wire-format signal.
type openAIResponseShapeKind int

const (
	openAIResponseShapeUnknown openAIResponseShapeKind = iota
	openAIResponseShapeChat
	openAIResponseShapeResponses
)

func openAIResponseShape(contentType string, body []byte) openAIResponseShapeKind {
	if conversation.IsSSEContentType(contentType) {
		// Responses API streams emit `response.*` event names; Chat
		// Completions streams use anonymous data lines wrapping
		// `chat.completion.chunk` objects. Sniff a small prefix
		// rather than parsing the whole stream — the first ~512 bytes
		// always carry the discriminating event line.
		prefix := body
		if len(prefix) > 512 {
			prefix = prefix[:512]
		}
		s := string(prefix)
		if strings.Contains(s, "event: response.") || strings.Contains(s, "response.output_item") {
			return openAIResponseShapeResponses
		}
		if strings.Contains(s, "chat.completion.chunk") {
			return openAIResponseShapeChat
		}
		return openAIResponseShapeUnknown
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return openAIResponseShapeUnknown
	}
	if _, ok := probe["choices"]; ok {
		return openAIResponseShapeChat
	}
	if _, ok := probe["output"]; ok {
		return openAIResponseShapeResponses
	}
	// `object` is the legacy disambiguator on Chat Completions
	// (object="chat.completion") and Responses (object="response").
	// Fall back to it when neither top-level array is present (e.g.
	// minimal responses without choices/output populated).
	if rawObj, ok := probe["object"]; ok {
		var obj string
		if err := json.Unmarshal(rawObj, &obj); err == nil {
			switch obj {
			case "chat.completion", "chat.completion.chunk":
				return openAIResponseShapeChat
			case "response":
				return openAIResponseShapeResponses
			}
		}
	}
	return openAIResponseShapeUnknown
}
