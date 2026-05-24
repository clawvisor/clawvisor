package llmproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// ContinuationToolResult is one synthetic tool_result the proxy is
// feeding back to the model. The text is rendered into the provider's
// tool_result block shape verbatim (no further escaping or wrapping).
type ContinuationToolResult struct {
	ToolUseID string
	Content   string
}

// ErrContinuationUnsupportedProvider signals that the proxy does not
// know how to build a continuation body for this provider. The caller
// treats it as a soft failure and falls back to the substitute-with
// rendering, which terminates the turn but surfaces the auto-approval
// text to the harness.
var ErrContinuationUnsupportedProvider = errors.New("llmproxy: continuation unsupported for provider")

// BuildContinuationBody constructs the request body the proxy POSTs
// upstream to continue the conversation after intercepting a tool_use
// and answering it locally. The new body is the original request body
// with messages[] (or the provider's equivalent) extended by (a) the
// assistant turn we just received from the upstream response, and (b)
// a synthetic user turn containing tool_result blocks for each
// intercepted tool. Other top-level fields (model, system, tools,
// max_tokens, stream, …) pass through unchanged.
//
// Only Anthropic is supported in the first cut; OpenAI providers
// return ErrContinuationUnsupportedProvider.
func BuildContinuationBody(
	provider conversation.Provider,
	contentType string,
	originalRequestBody []byte,
	upstreamResponseBody []byte,
	toolResults []ContinuationToolResult,
) ([]byte, error) {
	if len(toolResults) == 0 {
		return nil, errors.New("llmproxy: continuation requires at least one tool_result")
	}
	switch provider {
	case conversation.ProviderAnthropic:
		return buildAnthropicContinuationBody(contentType, originalRequestBody, upstreamResponseBody, toolResults)
	default:
		return nil, ErrContinuationUnsupportedProvider
	}
}

func buildAnthropicContinuationBody(
	contentType string,
	originalRequestBody []byte,
	upstreamResponseBody []byte,
	toolResults []ContinuationToolResult,
) ([]byte, error) {
	// Top-level original body. map[string]json.RawMessage preserves
	// every field byte-for-byte except messages (which we extend).
	var top map[string]json.RawMessage
	if err := json.Unmarshal(originalRequestBody, &top); err != nil {
		return nil, fmt.Errorf("continuation: parse original request body: %w", err)
	}
	messagesRaw, ok := top["messages"]
	if !ok {
		return nil, errors.New("continuation: original request has no messages field")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		return nil, fmt.Errorf("continuation: parse messages: %w", err)
	}

	assistantContent, err := conversation.ExtractAnthropicAssistantContent(contentType, upstreamResponseBody)
	if err != nil {
		return nil, fmt.Errorf("continuation: extract assistant turn: %w", err)
	}
	assistantTurn := map[string]any{
		"role":    "assistant",
		"content": assistantContent,
	}
	assistantRaw, err := json.Marshal(assistantTurn)
	if err != nil {
		return nil, fmt.Errorf("continuation: marshal assistant turn: %w", err)
	}

	userContent := make([]map[string]any, 0, len(toolResults))
	for _, tr := range toolResults {
		if strings.TrimSpace(tr.ToolUseID) == "" {
			continue
		}
		userContent = append(userContent, map[string]any{
			"type":        "tool_result",
			"tool_use_id": tr.ToolUseID,
			"content":     tr.Content,
		})
	}
	if len(userContent) == 0 {
		return nil, errors.New("continuation: no tool_result blocks to inject")
	}
	userTurn := map[string]any{
		"role":    "user",
		"content": userContent,
	}
	userRaw, err := json.Marshal(userTurn)
	if err != nil {
		return nil, fmt.Errorf("continuation: marshal user turn: %w", err)
	}

	messages = append(messages, json.RawMessage(assistantRaw), json.RawMessage(userRaw))
	mergedMessages, err := json.Marshal(messages)
	if err != nil {
		return nil, fmt.Errorf("continuation: marshal merged messages: %w", err)
	}
	top["messages"] = mergedMessages
	out, err := json.Marshal(top)
	if err != nil {
		return nil, fmt.Errorf("continuation: marshal continuation body: %w", err)
	}
	return out, nil
}
