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
	// Responses `input` carries the latest turn first when present.
	var input string
	if len(parsed.Input) > 0 && json.Unmarshal(parsed.Input, &input) == nil {
		return strings.TrimSpace(input)
	}
	var items []map[string]any
	if len(parsed.Input) > 0 && json.Unmarshal(parsed.Input, &items) == nil {
		for i := len(items) - 1; i >= 0; i-- {
			role, _ := items[i]["role"].(string)
			if role != "user" {
				continue
			}
			raw, _ := json.Marshal(items[i]["content"])
			return strings.TrimSpace(flattenOpenAITaskReplyContent(raw))
		}
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

// AssistantTextTurns walks BOTH the Responses `input` array AND the
// Chat `messages` array (in that order, matching the legacy
// secret-decision walker) and returns flattened assistant text turns
// most-recent first. Either array can be absent; both can coexist on
// a malformed body, and the latest-first contract picks the
// Responses entries first because that's where modern OpenAI
// harnesses (codex) place their history.
func (OpenAIInboundShape) AssistantTextTurns(body []byte) []string {
	var parsed struct {
		Messages []map[string]any `json:"messages"`
		Input    json.RawMessage  `json:"input"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	out := make([]string, 0, 8)
	var items []map[string]any
	if len(parsed.Input) > 0 && json.Unmarshal(parsed.Input, &items) == nil {
		for i := len(items) - 1; i >= 0; i-- {
			role, _ := items[i]["role"].(string)
			if role != "assistant" {
				continue
			}
			raw, _ := json.Marshal(items[i]["content"])
			out = append(out, flattenOpenAITaskReplyContent(raw))
		}
	}
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
