package llmproxy

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// AnthropicInboundShape implements conversation.InboundBodyShape for
// /v1/messages bodies. Methods delegate to internal walkers that
// existed before this abstraction was introduced; the type lets
// callers stop hand-rolling switch statements and route every
// per-provider body operation through one consistent dispatch.
type AnthropicInboundShape struct{}

func (AnthropicInboundShape) Name() conversation.Provider {
	return conversation.ProviderAnthropic
}

func (AnthropicInboundShape) HasAssistantTurn(body []byte) bool {
	return anthropicInboundHasAssistant(body)
}

func (AnthropicInboundShape) RecentHumanTurns(body []byte) []string {
	return extractAnthropicHumanTurns(body)
}

func (AnthropicInboundShape) LatestUserText(body []byte) string {
	var parsed struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	for i := len(parsed.Messages) - 1; i >= 0; i-- {
		if parsed.Messages[i].Role == "user" {
			return strings.TrimSpace(flattenAnthropicTaskReplyText(parsed.Messages[i].Content))
		}
	}
	return ""
}

// AssistantTextTurns returns flattened text for every assistant-role
// turn, most-recent first. Tool_use blocks are skipped — only the
// text content survives, matching the inline-switch semantics
// LatestAssistantSecretDecisionID used to use.
func (AnthropicInboundShape) AssistantTextTurns(body []byte) []string {
	var parsed struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	out := make([]string, 0, len(parsed.Messages))
	for i := len(parsed.Messages) - 1; i >= 0; i-- {
		if parsed.Messages[i].Role != "assistant" {
			continue
		}
		out = append(out, flattenAnthropicTaskReplyText(parsed.Messages[i].Content))
	}
	return out
}

func (AnthropicInboundShape) PrependAssistantText(contentType string, body []byte, text string) ([]byte, error) {
	return conversation.PrependAnthropicAssistantText(contentType, body, text)
}

// OpenAIInboundShape implements conversation.InboundBodyShape for
// OpenAI Chat Completions and Responses bodies. Sub-shape
// disambiguation happens per-method since each operation has its
// own preferred ordering (Responses input[] first, then Chat
// messages[]) — see method comments.
type OpenAIInboundShape struct{}

func (OpenAIInboundShape) Name() conversation.Provider {
	return conversation.ProviderOpenAI
}

func (OpenAIInboundShape) HasAssistantTurn(body []byte) bool {
	return openAIInboundHasAssistant(body)
}

func (OpenAIInboundShape) RecentHumanTurns(body []byte) []string {
	return extractOpenAIHumanTurns(body)
}

func (OpenAIInboundShape) LatestUserText(body []byte) string {
	var parsed struct {
		Messages []map[string]any `json:"messages"`
		Input    json.RawMessage  `json:"input"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	// Responses API (`input`) and Chat Completions (`messages`) are
	// mutually exclusive on the wire: a real OpenAI body has one or
	// the other. When Input is set it IS the conversation history;
	// falling back to Messages would risk returning a stale entry
	// from a malformed body where both arrays exist but only Input
	// reflects current state. Pick whichever is populated and walk
	// only that one.
	if len(parsed.Input) > 0 {
		var input string
		if json.Unmarshal(parsed.Input, &input) == nil {
			return strings.TrimSpace(input)
		}
		var items []map[string]any
		if json.Unmarshal(parsed.Input, &items) == nil {
			for i := len(items) - 1; i >= 0; i-- {
				role, _ := items[i]["role"].(string)
				if role != "user" {
					continue
				}
				raw, _ := json.Marshal(items[i]["content"])
				return strings.TrimSpace(flattenOpenAITaskReplyContent(raw))
			}
		}
		return ""
	}
	for i := len(parsed.Messages) - 1; i >= 0; i-- {
		role, _ := parsed.Messages[i]["role"].(string)
		if role != "user" {
			continue
		}
		raw, _ := json.Marshal(parsed.Messages[i]["content"])
		return strings.TrimSpace(flattenOpenAITaskReplyContent(raw))
	}
	return ""
}

// AssistantTextTurns returns flattened assistant text turns most-
// recent first. Responses (`input`) and Chat Completions (`messages`)
// are mutually exclusive on the wire — walking BOTH and concatenating
// would interleave stale `input` entries before fresh `messages`
// entries (or vice-versa) and produce a sequence that contradicts
// the latest-first contract. Pick whichever array is populated.
func (OpenAIInboundShape) AssistantTextTurns(body []byte) []string {
	var parsed struct {
		Messages []map[string]any `json:"messages"`
		Input    json.RawMessage  `json:"input"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	if len(parsed.Input) > 0 {
		var items []map[string]any
		if json.Unmarshal(parsed.Input, &items) != nil {
			return nil
		}
		out := make([]string, 0, len(items))
		for i := len(items) - 1; i >= 0; i-- {
			role, _ := items[i]["role"].(string)
			if role != "assistant" {
				continue
			}
			raw, _ := json.Marshal(items[i]["content"])
			out = append(out, flattenOpenAITaskReplyContent(raw))
		}
		return out
	}
	out := make([]string, 0, len(parsed.Messages))
	for i := len(parsed.Messages) - 1; i >= 0; i-- {
		role, _ := parsed.Messages[i]["role"].(string)
		if role != "assistant" {
			continue
		}
		raw, _ := json.Marshal(parsed.Messages[i]["content"])
		out = append(out, flattenOpenAITaskReplyContent(raw))
	}
	return out
}

func (OpenAIInboundShape) PrependAssistantText(contentType string, body []byte, text string) ([]byte, error) {
	switch conversation.OpenAIResponseShape(contentType, body) {
	case conversation.OpenAIResponseShapeChat:
		return conversation.PrependOpenAIChatAssistantText(contentType, body, text)
	case conversation.OpenAIResponseShapeResponses:
		return conversation.PrependOpenAIResponsesAssistantText(contentType, body, text)
	default:
		return body, nil
	}
}

// DefaultInboundShapeRegistry returns the canonical shape dispatch
// table. Mirrors DefaultResponseRegistry / DefaultInboundRegistry so
// all three legs route through a consistent abstraction.
func DefaultInboundShapeRegistry() *conversation.InboundShapeRegistry {
	return conversation.NewInboundShapeRegistry(
		AnthropicInboundShape{},
		OpenAIInboundShape{},
	)
}
