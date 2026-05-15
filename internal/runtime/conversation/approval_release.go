package conversation

import (
	"encoding/json"
	"net/http"
	"strings"
)

const ApprovalDeniedMessage = "Approval denied. The requested tool call was not performed."

type SyntheticApprovalResponse struct {
	ContentType string
	Body        []byte
}

func ApprovalReplyForProvider(provider Provider, body []byte) (verb, id string) {
	switch provider {
	case ProviderAnthropic:
		return AnthropicApprovalReply(body)
	case ProviderOpenAI:
		return OpenAIApprovalReply(body)
	default:
		return "", ""
	}
}

func AnthropicApprovalReply(body []byte) (verb, id string) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		return ParseApprovalReplyText(flattenAnthropicUserText(req.Messages[i].Content))
	}
	return "", ""
}

func SyntheticApprovalToolUseResponse(req *http.Request, provider Provider, requestBody []byte, allow bool, toolUseID, toolName string, toolInput map[string]any) (SyntheticApprovalResponse, bool) {
	return SyntheticApprovalToolUseResponseWithDenyMessage(req, provider, requestBody, allow, toolUseID, toolName, toolInput, ApprovalDeniedMessage)
}

func SyntheticApprovalToolUseResponseWithDenyMessage(req *http.Request, provider Provider, requestBody []byte, allow bool, toolUseID, toolName string, toolInput map[string]any, denyMessage string) (SyntheticApprovalResponse, bool) {
	denyMessage = strings.TrimSpace(denyMessage)
	if denyMessage == "" {
		denyMessage = ApprovalDeniedMessage
	}
	contentType := "application/json"
	var body []byte
	switch provider {
	case ProviderAnthropic:
		stream := AnthropicRequestWantsStream(requestBody)
		if allow {
			if stream {
				contentType = "text/event-stream"
				body = SynthAnthropicToolUseSSE("", "", "assistant", toolUseID, toolName, toolInput)
			} else {
				body = SynthAnthropicToolUseJSON("", "", "assistant", toolUseID, toolName, toolInput)
			}
		} else if stream {
			contentType = "text/event-stream"
			body = SynthAnthropicTextSSE("", "", "assistant", denyMessage)
		} else {
			body = SynthAnthropicTextJSON("", "", "assistant", denyMessage)
		}
	case ProviderOpenAI:
		stream := OpenAIRequestWantsStream(requestBody)
		if IsOpenAIChatCompletionsEndpoint(req) {
			if allow {
				if stream {
					contentType = "text/event-stream"
					body = SynthOpenAIChatToolCallSSE(toolUseID, toolName, toolInput)
				} else {
					body = SynthOpenAIChatToolCallJSON(toolUseID, toolName, toolInput)
				}
			} else if stream {
				contentType = "text/event-stream"
				body = SynthOpenAIChatTextSSE(denyMessage)
			} else {
				body = SynthOpenAIChatTextJSON(denyMessage)
			}
		} else if allow {
			if stream {
				contentType = "text/event-stream"
				body = SynthOpenAIResponsesFunctionCallSSE(toolUseID, toolName, toolInput)
			} else {
				body = SynthOpenAIResponsesFunctionCallJSON(toolUseID, toolName, toolInput)
			}
		} else if stream {
			contentType = "text/event-stream"
			body = SynthOpenAIResponsesTextSSE(denyMessage)
		} else {
			body = SynthOpenAIResponsesTextJSON(denyMessage)
		}
	default:
		return SyntheticApprovalResponse{}, false
	}
	return SyntheticApprovalResponse{ContentType: contentType, Body: body}, len(body) > 0
}

func flattenAnthropicUserText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var simple string
	if err := json.Unmarshal(raw, &simple); err == nil {
		return simple
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var out []string
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			out = append(out, block.Text)
		}
	}
	return strings.Join(out, "\n")
}
