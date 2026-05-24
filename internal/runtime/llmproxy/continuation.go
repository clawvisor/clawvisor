package llmproxy

import (
	"bytes"
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
// Provider routing:
//   - Anthropic: append assistant+user turns to messages[].
//   - OpenAI Chat Completions (original body has "messages"): append
//     assistant message with tool_calls + role:"tool" rows to messages[].
//   - OpenAI Responses API (original body has "input"): append the
//     output[] items + function_call_output items to input[],
//     promoting a string-form input to an array first.
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
	case conversation.ProviderOpenAI:
		shape := detectOpenAIShape(originalRequestBody)
		switch shape {
		case openAIShapeChat:
			return buildOpenAIChatContinuationBody(contentType, originalRequestBody, upstreamResponseBody, toolResults)
		case openAIShapeResponses:
			return buildOpenAIResponsesContinuationBody(contentType, originalRequestBody, upstreamResponseBody, toolResults)
		default:
			return nil, errors.New("llmproxy: cannot determine openai request shape (no messages or input field)")
		}
	default:
		return nil, ErrContinuationUnsupportedProvider
	}
}

// PrependAssistantNotice inserts a leading text block into the
// assistant turn of an upstream LLM response so the user sees a
// notice ("a task was auto-approved") in the same turn as the
// model's next actions. Dispatches by provider; for OpenAI it
// auto-detects the Chat Completions vs Responses shape from the
// response body. Returns the original body untouched (no error)
// when text is blank, the shape can't be determined, or the body
// is malformed — the notice is a UX nicety, not a correctness
// guarantee, and we prefer surfacing the model's output to losing
// it on a parse hiccup.
func PrependAssistantNotice(
	provider conversation.Provider,
	contentType string,
	body []byte,
	text string,
) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return body, nil
	}
	switch provider {
	case conversation.ProviderAnthropic:
		return conversation.PrependAnthropicAssistantText(contentType, body, text)
	case conversation.ProviderOpenAI:
		// Differentiate Chat Completions vs Responses by sniffing the
		// response body. Chat carries `choices[]`; Responses carries
		// `output[]` (and SSE Responses streams carry
		// `response.output_item.*` events).
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

type openAIShape int

const (
	openAIShapeUnknown openAIShape = iota
	openAIShapeChat
	openAIShapeResponses
)

// detectOpenAIShape inspects the original request body to decide
// which OpenAI API surface the harness was targeting. The two
// surfaces are mutually exclusive in practice: Chat Completions uses
// "messages", Responses API uses "input". A body carrying both is
// non-conforming; we prefer "messages" (Chat Completions) to match
// the older + more common shape.
func detectOpenAIShape(body []byte) openAIShape {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return openAIShapeUnknown
	}
	if _, ok := probe["messages"]; ok {
		return openAIShapeChat
	}
	if _, ok := probe["input"]; ok {
		return openAIShapeResponses
	}
	return openAIShapeUnknown
}

func buildOpenAIChatContinuationBody(
	contentType string,
	originalRequestBody []byte,
	upstreamResponseBody []byte,
	toolResults []ContinuationToolResult,
) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(originalRequestBody, &top); err != nil {
		return nil, fmt.Errorf("continuation: parse original chat request body: %w", err)
	}
	messagesRaw, ok := top["messages"]
	if !ok {
		return nil, errors.New("continuation: chat completions body missing messages")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		return nil, fmt.Errorf("continuation: parse chat messages: %w", err)
	}

	asst, err := conversation.ExtractOpenAIChatAssistantMessage(contentType, upstreamResponseBody)
	if err != nil {
		return nil, fmt.Errorf("continuation: extract chat assistant message: %w", err)
	}
	asstTurn := map[string]any{"role": "assistant"}
	// Chat Completions accepts content=null when tool_calls is the
	// only payload. Emit nil rather than an empty string so the
	// upstream's parser treats it as "no prose" rather than "empty
	// prose," which some versions of the API have validated against.
	if asst.Content != "" {
		asstTurn["content"] = asst.Content
	} else {
		asstTurn["content"] = nil
	}
	if len(asst.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(asst.ToolCalls))
		for _, c := range asst.ToolCalls {
			calls = append(calls, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": c.Arguments,
				},
			})
		}
		asstTurn["tool_calls"] = calls
	}
	asstRaw, err := json.Marshal(asstTurn)
	if err != nil {
		return nil, fmt.Errorf("continuation: marshal chat assistant turn: %w", err)
	}
	messages = append(messages, asstRaw)

	// One role:"tool" message per tool_call_id. Chat Completions
	// requires that every tool_call in the prior assistant message be
	// resolved by a matching tool message before the model will
	// respond, so we emit exactly len(toolResults) of them.
	for _, tr := range toolResults {
		if strings.TrimSpace(tr.ToolUseID) == "" {
			continue
		}
		toolRow := map[string]any{
			"role":         "tool",
			"tool_call_id": tr.ToolUseID,
			"content":      tr.Content,
		}
		raw, err := json.Marshal(toolRow)
		if err != nil {
			return nil, fmt.Errorf("continuation: marshal chat tool row: %w", err)
		}
		messages = append(messages, raw)
	}

	mergedMessages, err := json.Marshal(messages)
	if err != nil {
		return nil, fmt.Errorf("continuation: marshal merged chat messages: %w", err)
	}
	top["messages"] = mergedMessages
	out, err := json.Marshal(top)
	if err != nil {
		return nil, fmt.Errorf("continuation: marshal chat continuation body: %w", err)
	}
	return out, nil
}

func buildOpenAIResponsesContinuationBody(
	contentType string,
	originalRequestBody []byte,
	upstreamResponseBody []byte,
	toolResults []ContinuationToolResult,
) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(originalRequestBody, &top); err != nil {
		return nil, fmt.Errorf("continuation: parse original responses request body: %w", err)
	}
	rawInput, ok := top["input"]
	if !ok {
		return nil, errors.New("continuation: responses body missing input")
	}
	// `input` may be a JSON string (the convenience form, e.g.
	// `"input":"make a file"`) or an array of items. Promote the
	// string to a single user-message item so we can append to it
	// uniformly.
	var inputItems []json.RawMessage
	var asString string
	if err := json.Unmarshal(rawInput, &asString); err == nil {
		promoted := map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": asString},
			},
		}
		raw, err := json.Marshal(promoted)
		if err != nil {
			return nil, fmt.Errorf("continuation: promote string input: %w", err)
		}
		inputItems = []json.RawMessage{raw}
	} else if err := json.Unmarshal(rawInput, &inputItems); err != nil {
		return nil, fmt.Errorf("continuation: parse responses input array: %w", err)
	}

	output, err := conversation.ExtractOpenAIResponsesOutput(contentType, upstreamResponseBody)
	if err != nil {
		return nil, fmt.Errorf("continuation: extract responses output: %w", err)
	}
	inputItems = append(inputItems, output.Items...)

	// function_call_output items per tool_result. The Responses API
	// keys these off `call_id`, not the function_call's `id`, so the
	// tool_use_id we plumbed through must already be the call_id (the
	// OpenAI response rewriter normalizes to call_id when emitting
	// tool_uses to the evaluator — see openai_response.go).
	for _, tr := range toolResults {
		if strings.TrimSpace(tr.ToolUseID) == "" {
			continue
		}
		fco := map[string]any{
			"type":    "function_call_output",
			"call_id": tr.ToolUseID,
			"output":  tr.Content,
		}
		raw, err := json.Marshal(fco)
		if err != nil {
			return nil, fmt.Errorf("continuation: marshal function_call_output: %w", err)
		}
		inputItems = append(inputItems, raw)
	}

	mergedInput, err := json.Marshal(inputItems)
	if err != nil {
		return nil, fmt.Errorf("continuation: marshal merged input: %w", err)
	}
	top["input"] = mergedInput
	out, err := json.Marshal(top)
	if err != nil {
		return nil, fmt.Errorf("continuation: marshal responses continuation body: %w", err)
	}
	return out, nil
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
