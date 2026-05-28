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
	if blocker := firstBuiltInToolItemType(resp.Output); blocker != "" {
		return nil, fmt.Errorf("%w: item type %q", ErrResponsesContinuationHasBuiltInToolItem, blocker)
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

// firstBuiltInToolItemType returns the type name of the first output
// item whose type the Responses API does not accept on input[]. Used
// to short-circuit ExtractOpenAIResponsesOutput before we build a
// continuation request that would 400 upstream.
func firstBuiltInToolItemType(items []json.RawMessage) string {
	for _, raw := range items {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			continue
		}
		if isResponsesItemContinuationBlocker(probe.Type) {
			return probe.Type
		}
	}
	return ""
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
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw.Item, &probe); err == nil && isResponsesItemContinuationBlocker(probe.Type) {
			return nil, fmt.Errorf("%w: item type %q", ErrResponsesContinuationHasBuiltInToolItem, probe.Type)
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
	case
		"message", "function_call", "custom_tool_call",
		// Reasoning items carry encrypted_content / summary that
		// o1/o3/o4-mini-style extended-thinking responses need
		// round-tripped so the model can continue its chain across
		// the synthetic function_call_output we inject.
		"reasoning":
		// Drop response-only fields. `status` is the load-bearing
		// one; other fields like `id`, `call_id`, `arguments`,
		// `output`, etc. are input-acceptable.
		delete(probe, "status")
		out, err := json.Marshal(probe)
		if err != nil {
			return nil, false
		}
		return out, true
	case
		// Built-in tool call items. These are output-only on the
		// Responses API request shape; re-sending them in input[]
		// 400s the continuation forward. We could drop them
		// silently (which loses grounding) or refuse continuation
		// (which preserves the fail-closed posture and surfaces
		// SubstituteWith to the harness). isResponsesItemContinuationBlocker
		// marks these so the extractor can short-circuit before we
		// build a doomed continuation body.
		"web_search_call",
		"file_search_call",
		"code_interpreter_call",
		"image_generation_call",
		"mcp_call",
		"mcp_list_tools",
		"mcp_approval_request",
		"local_shell_call":
		return nil, false
	default:
		// Genuinely unknown item types we have no model for. Drop
		// rather than re-emit a shape the upstream may reject. If
		// future Responses-API item types break this branch in
		// production, add them above with the same status-strip
		// pattern.
		return nil, false
	}
}

// isResponsesItemContinuationBlocker reports whether the given
// Responses-API output item type is one whose presence in the
// upstream's output means we cannot safely build a continuation
// request. These are the built-in tool call items that the
// Responses API rejects on input[]; if the model used any of them
// in the same turn it emitted POST /api/control/tasks, the
// continuation forward would 400. The extractor uses this to
// short-circuit with a typed error so tryContinuation falls back
// to SubstituteWith instead of issuing a doomed request.
func isResponsesItemContinuationBlocker(typ string) bool {
	switch typ {
	case
		"web_search_call",
		"file_search_call",
		"code_interpreter_call",
		"image_generation_call",
		"mcp_call",
		"mcp_list_tools",
		"mcp_approval_request",
		"local_shell_call":
		return true
	}
	return false
}

// ErrResponsesContinuationHasBuiltInToolItem is returned by
// ExtractOpenAIResponsesOutput when the upstream's output contains
// at least one built-in tool call item the Responses API rejects on
// input[]. The lite-proxy treats this as a soft failure: tryContinuation
// returns it as an error, the handler falls back to the original
// processed result, and the harness sees the SubstituteWith
// terminal text instead of a continuation that would have 400ed.
var ErrResponsesContinuationHasBuiltInToolItem = fmt.Errorf("conversation: openai responses output contains built-in tool call item; continuation unsupported")
