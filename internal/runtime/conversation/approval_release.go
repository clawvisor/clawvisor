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
	userIdx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			userIdx = i
			break
		}
	}
	if userIdx < 0 {
		return "", ""
	}
	verb, id = ParseApprovalReplyText(flattenAnthropicUserText(req.Messages[userIdx].Content))
	if verb == "" {
		// No plain-text approval reply on the user turn. Try the
		// AskUserQuestion path: the inline-approval intercept may
		// have substituted the model's tool_use with a picker
		// call, in which case the user's choice arrives as a
		// tool_result block (not free text). All downstream
		// callers — lite-proxy body editor, runtime proxy
		// parseApprovalReplyForProvider, ad-hoc tooling — see
		// AskUserQuestion releases via this fallback.
		return anthropicAskUserQuestionApprovalReply(body)
	}
	if id != "" {
		return verb, id
	}
	// Bare reply (e.g. "y"): scan back through assistant messages for the
	// most recent approval-ID marker. Without this, a "y" lands on whatever
	// hold the cache picks LIFO — which can be the wrong agent's when
	// several share a Clawvisor token. The marker pins the reply to the
	// specific approval prompt this transcript is looking at.
	for i := userIdx - 1; i >= 0; i-- {
		if req.Messages[i].Role != "assistant" {
			continue
		}
		if marker := FindLatestApprovalIDMarker(flattenAnthropicUserText(req.Messages[i].Content)); marker != "" {
			return verb, marker
		}
	}
	return verb, ""
}

// SyntheticToolCall is one tool_use entry in a (possibly multi-block)
// synthetic approval-release response. The release path emits one of
// these per held tool_use when the user approves a coalesced hold.
type SyntheticToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

func SyntheticApprovalToolUseResponse(req *http.Request, provider Provider, requestBody []byte, allow bool, toolUseID, toolName string, toolInput map[string]any) (SyntheticApprovalResponse, bool) {
	return SyntheticApprovalToolUseResponseWithDenyMessage(req, provider, requestBody, allow, toolUseID, toolName, toolInput, ApprovalDeniedMessage)
}

func SyntheticApprovalToolUseResponseWithDenyMessage(req *http.Request, provider Provider, requestBody []byte, allow bool, toolUseID, toolName string, toolInput map[string]any, denyMessage string) (SyntheticApprovalResponse, bool) {
	calls := []SyntheticToolCall{{ID: toolUseID, Name: toolName, Input: toolInput}}
	return SyntheticApprovalToolUsesResponseWithDenyMessage(req, provider, requestBody, allow, calls, denyMessage)
}

// SyntheticApprovalToolUsesResponseWithDenyMessage builds a synthetic
// upstream response that carries N tool_use blocks on approve (or a
// single text block on deny). When len(calls) == 1 the wire shape is
// byte-identical to the single-call helper. When len(calls) > 1 the
// shape is a multi-block assistant turn (Anthropic content[], OpenAI
// Responses output[], OpenAI Chat tool_calls[]).
//
// Used by the coalesced-approval release path: one user yes/no covers
// every held tool_use in the turn, so the synthetic response must
// surface every approved call back to the harness for execution.
func SyntheticApprovalToolUsesResponseWithDenyMessage(req *http.Request, provider Provider, requestBody []byte, allow bool, calls []SyntheticToolCall, denyMessage string) (SyntheticApprovalResponse, bool) {
	denyMessage = strings.TrimSpace(denyMessage)
	if denyMessage == "" {
		denyMessage = ApprovalDeniedMessage
	}
	if allow && len(calls) == 0 {
		return SyntheticApprovalResponse{}, false
	}
	contentType := "application/json"
	var body []byte
	switch provider {
	case ProviderAnthropic:
		stream := AnthropicRequestWantsStream(requestBody)
		if allow {
			if stream {
				contentType = "text/event-stream"
				body = SynthAnthropicToolUsesSSE("", "", "assistant", calls)
			} else {
				body = SynthAnthropicToolUsesJSON("", "", "assistant", calls)
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
					body = SynthOpenAIChatToolCallsSSE(calls)
				} else {
					body = SynthOpenAIChatToolCallsJSON(calls)
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
				body = SynthOpenAIResponsesFunctionCallsSSE(calls)
			} else {
				body = SynthOpenAIResponsesFunctionCallsJSON(calls)
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
