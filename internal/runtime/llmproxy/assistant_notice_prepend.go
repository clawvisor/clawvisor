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
		return openAIStreamShape(body)
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

// openAIStreamShape inspects the FIRST SSE event in body and classifies
// the stream by its event-name prefix line (Responses) or its data
// payload's `object` field (Chat). Substring searches across the full
// stream are unreliable because model-authored content can include
// these tokens; the first event is always provider envelope metadata
// (response.created / chat.completion.chunk with role="assistant") and
// never carries free-form model text.
func openAIStreamShape(body []byte) openAIResponseShapeKind {
	normalized := strings.ReplaceAll(string(body), "\r\n", "\n")
	for _, block := range strings.Split(normalized, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var eventName, dataLine string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		// Responses API events always start with the `response.`
		// prefix on the FIRST event of the stream. Chat Completions
		// has no `event:` lines.
		if strings.HasPrefix(eventName, "response.") {
			return openAIResponseShapeResponses
		}
		if eventName != "" {
			// Event name set to something that's NOT a response.*
			// prefix — probably Anthropic or another shape. Skip to
			// the next event in case the first one was a comment /
			// keep-alive.
			continue
		}
		// Chat Completions: parse the data line's top-level `object`
		// field. Don't substring-search — model text in a later delta
		// could legitimately contain "chat.completion.chunk".
		if dataLine == "" || dataLine == "[DONE]" {
			continue
		}
		var probe struct {
			Object string `json:"object"`
		}
		if err := json.Unmarshal([]byte(dataLine), &probe); err != nil {
			continue
		}
		switch probe.Object {
		case "chat.completion", "chat.completion.chunk":
			return openAIResponseShapeChat
		case "response":
			return openAIResponseShapeResponses
		}
	}
	return openAIResponseShapeUnknown
}
