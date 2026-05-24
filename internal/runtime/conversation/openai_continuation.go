package conversation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// OpenAIChatAssistantMessage is the structured assistant turn the
// Chat Completions continuation builder needs to re-send to the
// upstream. It mirrors the message shape OpenAI accepts on
// /v1/chat/completions requests: optional text content, plus zero or
// more tool_calls keyed by id.
type OpenAIChatAssistantMessage struct {
	Content   string                  `json:"content,omitempty"`
	ToolCalls []OpenAIChatToolCallRef `json:"tool_calls,omitempty"`
}

// OpenAIChatToolCallRef is one tool_call inside an assistant message.
// Arguments are kept as the raw JSON-encoded string the upstream
// emitted so the continuation request round-trips byte-for-byte.
type OpenAIChatToolCallRef struct {
	ID        string
	Name      string
	Arguments string
}

// ExtractOpenAIChatAssistantMessage reconstructs the assistant turn
// from a Chat Completions response (JSON or SSE). The returned value
// can be serialized straight into the messages[] array of a
// continuation request — the continuation builder takes care of
// appending the role:"tool" rows that match each tool_call.
func ExtractOpenAIChatAssistantMessage(contentType string, body []byte) (*OpenAIChatAssistantMessage, error) {
	if isSSE(contentType) || looksLikeSSE(body) {
		return extractOpenAIChatAssistantMessageSSE(body)
	}
	return extractOpenAIChatAssistantMessageJSON(body)
}

func extractOpenAIChatAssistantMessageJSON(body []byte) (*OpenAIChatAssistantMessage, error) {
	var resp openAIChatCompletionsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("conversation: parse openai chat JSON: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("conversation: openai chat response has no choices")
	}
	msg := resp.Choices[0].Message
	out := &OpenAIChatAssistantMessage{
		Content: flattenOpenAIContentFromAny(msg.Content),
	}
	for _, call := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, OpenAIChatToolCallRef{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		})
	}
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return nil, fmt.Errorf("conversation: openai chat response has no content or tool_calls")
	}
	return out, nil
}

func extractOpenAIChatAssistantMessageSSE(body []byte) (*OpenAIChatAssistantMessage, error) {
	lines := strings.Split(string(body), "\n")
	type pendingCall struct {
		id   string
		name string
		args strings.Builder
	}
	pending := map[int]*pendingCall{}
	var text strings.Builder
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Choices []openAIChatChoice `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		// Limit to choice index 0. The JSON extractor already uses
		// Choices[0] only; mirroring that here keeps shapes consistent
		// across wire formats. Merging multiple choices into a single
		// continuation would either concatenate alternative
		// completions (when the harness sets n>1) or collide on
		// identical tool_call indices across choices, producing a
		// malformed second request.
		for _, choice := range event.Choices {
			if choice.Index != 0 {
				continue
			}
			if txt := flattenOpenAIContentFromAny(choice.Delta.Content); txt != "" {
				text.WriteString(txt)
			}
			for _, tc := range choice.Delta.ToolCalls {
				pc := pending[tc.Index]
				if pc == nil {
					pc = &pendingCall{}
					pending[tc.Index] = pc
				}
				if tc.ID != "" {
					pc.id = tc.ID
				}
				if tc.Function.Name != "" {
					pc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					pc.args.WriteString(tc.Function.Arguments)
				}
			}
		}
	}
	out := &OpenAIChatAssistantMessage{Content: text.String()}
	indexes := make([]int, 0, len(pending))
	for i := range pending {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		pc := pending[i]
		if pc.id == "" {
			continue
		}
		out.ToolCalls = append(out.ToolCalls, OpenAIChatToolCallRef{
			ID:        pc.id,
			Name:      pc.name,
			Arguments: pc.args.String(),
		})
	}
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return nil, fmt.Errorf("conversation: openai chat SSE yielded no content or tool_calls")
	}
	return out, nil
}

// OpenAIResponsesOutputItems carries the structured output array the
// Responses API continuation builder needs to splice back into the
// request's input[] field. Items are kept as json.RawMessage so the
// upstream-emitted shape (including any fields we don't model) is
// preserved verbatim.
type OpenAIResponsesOutputItems struct {
	Items []json.RawMessage
}

// ExtractOpenAIResponsesOutput reconstructs the output[] array from a
// Responses API response. Handles JSON and SSE wire formats. Items
// are restored to the shape the upstream accepts in input[] on a
// follow-up request (we strip transient fields like `status` that the
// API rejects on input).
func ExtractOpenAIResponsesOutput(contentType string, body []byte) (*OpenAIResponsesOutputItems, error) {
	if isSSE(contentType) || looksLikeSSE(body) {
		return extractOpenAIResponsesOutputSSE(body)
	}
	return extractOpenAIResponsesOutputJSON(body)
}

func extractOpenAIResponsesOutputJSON(body []byte) (*OpenAIResponsesOutputItems, error) {
	var resp struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("conversation: parse openai responses JSON: %w", err)
	}
	if len(resp.Output) == 0 {
		return nil, fmt.Errorf("conversation: openai responses has no output items")
	}
	out := &OpenAIResponsesOutputItems{}
	for _, raw := range resp.Output {
		cleaned, ok := sanitizeResponsesItemForInput(raw)
		if !ok {
			continue
		}
		out.Items = append(out.Items, cleaned)
	}
	if len(out.Items) == 0 {
		return nil, fmt.Errorf("conversation: openai responses output had no usable items after sanitize")
	}
	return out, nil
}

func extractOpenAIResponsesOutputSSE(body []byte) (*OpenAIResponsesOutputItems, error) {
	events, err := parseSSEEvents(body)
	if err != nil {
		return nil, fmt.Errorf("conversation: parse openai responses SSE: %w", err)
	}
	// response.output_item.done carries the fully-formed item; that's
	// the cleanest signal to extract from. function_call arguments may
	// have arrived as deltas, but the `.done` event contains the final
	// assembled item with arguments present.
	type indexed struct {
		idx  int
		item json.RawMessage
	}
	var byIndex []indexed
	for _, ev := range events {
		if ev.Event != "response.output_item.done" {
			continue
		}
		var raw struct {
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &raw); err != nil {
			continue
		}
		cleaned, ok := sanitizeResponsesItemForInput(raw.Item)
		if !ok {
			continue
		}
		byIndex = append(byIndex, indexed{idx: raw.OutputIndex, item: cleaned})
	}
	if len(byIndex) == 0 {
		return nil, fmt.Errorf("conversation: openai responses SSE yielded no output_item.done events")
	}
	// Sort by output_index so the items end up in the order the
	// upstream emitted them; this matters for the continuation request
	// because the model expects function_call to precede its
	// function_call_output.
	sort.Slice(byIndex, func(i, j int) bool { return byIndex[i].idx < byIndex[j].idx })
	out := &OpenAIResponsesOutputItems{}
	for _, i := range byIndex {
		out.Items = append(out.Items, i.item)
	}
	return out, nil
}

// sanitizeResponsesItemForInput strips fields the Responses API
// rejects when an item is re-sent on the request input[] (e.g.
// `status`, which is response-only). Returns (cleaned, true) when the
// item is a known input-acceptable type; (nil, false) when we don't
// know how to round-trip it. Today: message, function_call,
// custom_tool_call.
func sanitizeResponsesItemForInput(raw json.RawMessage) (json.RawMessage, bool) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, false
	}
	var typ string
	_ = json.Unmarshal(probe["type"], &typ)
	switch typ {
	case "message", "function_call", "custom_tool_call":
		// Drop response-only fields. `status` is the load-bearing one;
		// the others are belt-and-suspenders.
		delete(probe, "status")
		out, err := json.Marshal(probe)
		if err != nil {
			return nil, false
		}
		return out, true
	case "reasoning":
		// o1/o3/o4-mini extended-thinking responses include reasoning
		// items carrying `encrypted_content` (or `summary`) that the
		// upstream needs back on the continuation request so the model
		// can preserve its chain of thought across the synthetic
		// function_call_output we're about to inject. Dropping these
		// either 400s the request (some models require the reasoning
		// item to immediately precede the function_call) or breaks
		// the model's reasoning continuity. Strip only the response-
		// only `status` field and re-emit verbatim.
		delete(probe, "status")
		out, err := json.Marshal(probe)
		if err != nil {
			return nil, false
		}
		return out, true
	default:
		// Truly unknown / response-only item types (web_search_call,
		// image_generation_call, …) are dropped on the continuation:
		// re-sending them as input[] items would likely 400 from the
		// upstream.
		return nil, false
	}
}
