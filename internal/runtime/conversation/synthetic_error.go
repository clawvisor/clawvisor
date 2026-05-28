package conversation

import (
	"net/http"
	"strings"
)

// SyntheticErrorResponse builds a harness-shaped response that carries
// an error message as an assistant text turn. The harness renders the
// message inline so the user can read it and retry, instead of seeing
// the CLI's generic "model may not exist" fallback that fires when the
// proxy returns a non-harness-shaped HTTP error.
//
// The synthesized response uses HTTP 200 on the wire — harness clients
// only parse content from successful responses. The original error
// classification stays in audit logs and slog for operators; the wire
// is for the user.
//
// Returns ok=false when the provider is unsupported (caller should
// fall back to a plain JSON error) or when message is empty.
func SyntheticErrorResponse(req *http.Request, provider Provider, requestBody []byte, message string) (SyntheticApprovalResponse, bool) {
	message = strings.TrimSpace(message)
	if message == "" {
		return SyntheticApprovalResponse{}, false
	}
	contentType := "application/json"
	var body []byte
	switch provider {
	case ProviderAnthropic:
		if AnthropicRequestWantsStream(requestBody) {
			contentType = "text/event-stream"
			body = SynthAnthropicTextSSE("", "", "assistant", message)
		} else {
			body = SynthAnthropicTextJSON("", "", "assistant", message)
		}
	case ProviderOpenAI:
		stream := OpenAIRequestWantsStream(requestBody)
		if IsOpenAIChatCompletionsEndpoint(req) {
			if stream {
				contentType = "text/event-stream"
				body = SynthOpenAIChatTextSSE(message)
			} else {
				body = SynthOpenAIChatTextJSON(message)
			}
		} else if stream {
			contentType = "text/event-stream"
			body = SynthOpenAIResponsesTextSSE(message)
		} else {
			body = SynthOpenAIResponsesTextJSON(message)
		}
	default:
		return SyntheticApprovalResponse{}, false
	}
	if len(body) == 0 {
		return SyntheticApprovalResponse{}, false
	}
	return SyntheticApprovalResponse{ContentType: contentType, Body: body}, true
}
