package llmproxy

import (
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/bodytransform"
)

type SanitizeInboundRequest = bodytransform.SanitizeInboundRequest
type SanitizeInboundResult = bodytransform.SanitizeInboundResult

const ClawvisorManagedMarker = bodytransform.ClawvisorManagedMarker

func SanitizeAnthropicRequest(body []byte) ([]byte, bool, error) {
	return bodytransform.SanitizeAnthropicRequest(body)
}

func SanitizeInboundHistory(req SanitizeInboundRequest) (SanitizeInboundResult, error) {
	return bodytransform.SanitizeInboundHistory(req)
}

func NewSanitizeInboundRequest(provider conversation.Provider, body []byte, resolverBaseURL, controlBaseURL string) SanitizeInboundRequest {
	return SanitizeInboundRequest{
		Provider:        provider,
		Body:            body,
		ResolverBaseURL: resolverBaseURL,
		ControlBaseURL:  controlBaseURL,
	}
}
