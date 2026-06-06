package conversation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"
)

type OpenAIResponseRewriter struct{}

func (OpenAIResponseRewriter) Name() Provider { return ProviderOpenAI }

func (OpenAIResponseRewriter) MatchesResponse(req *http.Request, resp *http.Response) bool {
	return req != nil && resp != nil && matchOpenAIEndpoint(req)
}

func (rw OpenAIResponseRewriter) Rewrite(body []byte, contentType string, eval ToolUseEvaluator) (RewriteResult, error) {
	switch {
	case isOpenAIChatCompletionsEndpointFromBody(contentType, body):
		return rw.rewriteChatCompletions(body, contentType, eval)
	case isOpenAIResponsesBody(body):
		return rw.rewriteResponses(body, contentType, eval)
	default:
		if isSSE(contentType) || looksLikeSSE(body) {
			if bytes.Contains(body, []byte("response.output_item.added")) || bytes.Contains(body, []byte("response.function_call_arguments")) {
				return rw.rewriteResponses(body, contentType, eval)
			}
			return rw.rewriteChatCompletions(body, contentType, eval)
		}
		return RewriteResult{Body: body}, nil
	}
}

// rewriteResponses picks the SSE vs JSON path. Content-Type is the primary
// signal but isn't always present (some upstreams elide it for streamed
// responses, and proxy hops may strip it); fall back to body sniffing.
func (rw OpenAIResponseRewriter) rewriteResponses(body []byte, contentType string, eval ToolUseEvaluator) (RewriteResult, error) {
	if isSSE(contentType) || looksLikeSSE(body) {
		return rw.rewriteResponsesSSE(body, eval)
	}
	return rw.rewriteResponsesJSON(body, eval)
}

func (rw OpenAIResponseRewriter) rewriteChatCompletions(body []byte, contentType string, eval ToolUseEvaluator) (RewriteResult, error) {
	if isSSE(contentType) || looksLikeSSE(body) {
		return rw.rewriteChatCompletionsSSE(body, eval)
	}
	return rw.rewriteChatCompletionsJSON(body, eval)
}

// looksLikeSSE sniffs the body for an SSE framing pattern. Used as a
// fallback when the Content-Type header is missing — happens with some
// upstream transports and shows up here as empty contentType, which would
// otherwise route an SSE body through the JSON path (json.Unmarshal fails,
// no tool_uses get extracted, no rewrites fire).
func looksLikeSSE(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	head := body
	if len(head) > 4096 {
		head = head[:4096]
	}
	return bytes.HasPrefix(bytes.TrimLeft(head, "\r\n "), []byte("event:")) ||
		bytes.HasPrefix(bytes.TrimLeft(head, "\r\n "), []byte("data:")) ||
		bytes.Contains(head, []byte("\nevent: response."))
}

type openAIResponsesJSON struct {
	ID         string                     `json:"id,omitempty"`
	Object     string                     `json:"object,omitempty"`
	Model      string                     `json:"model,omitempty"`
	Output     []openAIResponseOutputItem `json:"output,omitempty"`
	OutputText string                     `json:"output_text,omitempty"`
}

type openAIResponseOutputItem struct {
	ID        string                  `json:"id,omitempty"`
	Type      string                  `json:"type"`
	Role      string                  `json:"role,omitempty"`
	Status    string                  `json:"status,omitempty"`
	CallID    string                  `json:"call_id,omitempty"`
	Name      string                  `json:"name,omitempty"`
	Arguments any                     `json:"arguments,omitempty"`
	Input     any                     `json:"input,omitempty"`
	Content   []openAIResponseContent `json:"content,omitempty"`
}

type openAIResponseContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (rw OpenAIResponseRewriter) rewriteResponsesJSON(body []byte, eval ToolUseEvaluator) (RewriteResult, error) {
	var resp openAIResponsesJSON
	if err := json.Unmarshal(body, &resp); err != nil {
		return RewriteResult{Body: body}, nil
	}
	var (
		decisions    []ToolUseDecisionRecord
		frags        []assistantFragment
		anyBlocked   bool
		anyRewritten bool
		index        int
	)
	newOutput := make([]openAIResponseOutputItem, 0, len(resp.Output))
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if (part.Type == "output_text" || part.Type == "text") && part.Text != "" {
					frags = append(frags, assistantFragment{Text: part.Text})
				}
			}
			newOutput = append(newOutput, item)
		case "function_call":
			args := stringifyOpenAIArguments(item.Arguments)
			tu := ToolUse{
				ID:    firstNonEmpty(item.CallID, item.ID),
				Index: index,
				Name:  item.Name,
				Input: rawIfJSONOpenAI(args),
			}
			index++
			verdict := eval(tu)
			decisions = append(decisions, ToolUseDecisionRecord{
				ToolUse:          tu,
				Verdict:          verdict,
				ToolInputPreview: MakeToolInputPreview(tu.Input),
			})
			finalArgs := tu.Input
			if !verdict.Allowed {
				anyBlocked = true
				txt := verdict.SubstituteWith
				if txt == "" && !verdict.SuppressSubstituteText {
					reason := verdict.Reason
					if reason == "" {
						reason = "blocked by policy"
					}
					txt = fmt.Sprintf("Tool '%s' was blocked by Clawvisor policy: %s", item.Name, reason)
				}
				if txt != "" {
					newOutput = append(newOutput, openAIResponseOutputItem{
						ID:     "msg_" + tu.ID,
						Type:   "message",
						Role:   "assistant",
						Status: "completed",
						Content: []openAIResponseContent{{
							Type: "output_text",
							Text: txt,
						}},
					})
				}
			} else {
				if len(verdict.RewriteInput) > 0 {
					item.Arguments = string(verdict.RewriteInput)
					finalArgs = verdict.RewriteInput
					anyRewritten = true
				}
				newOutput = append(newOutput, item)
			}
			frags = append(frags, assistantFragment{IsTool: true, ToolName: item.Name, ToolArgs: finalArgs})
		case "custom_tool_call":
			tu, ok := toolUseFromOpenAICustomToolCall(item, index)
			if !ok {
				newOutput = append(newOutput, item)
				continue
			}
			index++
			verdict := eval(tu)
			decisions = append(decisions, ToolUseDecisionRecord{
				ToolUse:          tu,
				Verdict:          verdict,
				ToolInputPreview: MakeToolInputPreview(tu.Input),
			})
			if !verdict.Allowed {
				anyBlocked = true
				txt := verdict.SubstituteWith
				if txt == "" && !verdict.SuppressSubstituteText {
					reason := verdict.Reason
					if reason == "" {
						reason = "blocked by policy"
					}
					txt = fmt.Sprintf("Tool '%s' was blocked by Clawvisor policy: %s", tu.Name, reason)
				}
				if txt != "" {
					newOutput = append(newOutput, openAIResponseOutputItem{
						ID:     "msg_" + tu.ID,
						Type:   "message",
						Role:   "assistant",
						Status: "completed",
						Content: []openAIResponseContent{{
							Type: "output_text",
							Text: txt,
						}},
					})
				}
			} else {
				newOutput = append(newOutput, item)
			}
			frags = append(frags, assistantFragment{IsTool: true, ToolName: tu.Name, ToolArgs: tu.Input})
		default:
			newOutput = append(newOutput, item)
		}
	}
	turn := assistantTurnFromFragments(frags, decisions)
	if anyBlocked || anyRewritten {
		resp.Output = newOutput
		var outputTextParts []string
		for _, o := range newOutput {
			if o.Type == "message" {
				for _, c := range o.Content {
					if c.Text != "" {
						outputTextParts = append(outputTextParts, c.Text)
					}
				}
			}
		}
		resp.OutputText = strings.Join(outputTextParts, "\n\n")
		rewritten, err := json.Marshal(resp)
		if err != nil {
			return RewriteResult{}, fmt.Errorf("openai responses: marshal rewritten response: %w", err)
		}
		return RewriteResult{Body: rewritten, Decisions: decisions, Rewritten: true, AssistantTurn: turn}, nil
	}
	return RewriteResult{Body: body, Decisions: decisions, AssistantTurn: turn}, nil
}

func (rw OpenAIResponseRewriter) rewriteResponsesSSE(body []byte, eval ToolUseEvaluator) (RewriteResult, error) {
	events, err := parseSSEEvents(body)
	if err != nil {
		return RewriteResult{Body: body}, nil
	}
	type pendingCall struct {
		itemID      string
		callID      string
		name        string
		outputIndex int
		arguments   strings.Builder
	}
	pending := map[string]*pendingCall{}
	textByIndex := map[int]*strings.Builder{}
	var decisions []ToolUseDecisionRecord
	var frags []assistantFragment
	// orderedItems preserves the order each output item completed in,
	// with enough metadata to re-emit a synthesized SSE response if
	// the rewriter mutates one or more function_call arguments.
	var orderedItems []orderedResponsesItem
	anyBlocked := false
	anyRewritten := false
	index := 0
	for _, event := range events {
		switch event.Event {
		case "response.output_item.added":
			var raw struct {
				OutputIndex int                      `json:"output_index"`
				Item        openAIResponseOutputItem `json:"item"`
			}
			if err := json.Unmarshal([]byte(event.Data), &raw); err != nil {
				continue
			}
			switch raw.Item.Type {
			case "message":
				if _, ok := textByIndex[raw.OutputIndex]; !ok {
					textByIndex[raw.OutputIndex] = &strings.Builder{}
				}
			case "function_call":
				pc := &pendingCall{
					itemID:      raw.Item.ID,
					callID:      firstNonEmpty(raw.Item.CallID, raw.Item.ID),
					name:        raw.Item.Name,
					outputIndex: raw.OutputIndex,
				}
				if args := stringifyOpenAIArguments(raw.Item.Arguments); args != "" {
					pc.arguments.WriteString(args)
				}
				pending[raw.Item.ID] = pc
			}
		case "response.function_call_arguments.delta":
			var raw struct {
				ItemID string `json:"item_id"`
				Delta  string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(event.Data), &raw); err != nil {
				continue
			}
			if pc := pending[raw.ItemID]; pc != nil {
				pc.arguments.WriteString(raw.Delta)
			}
		case "response.function_call_arguments.done":
			var raw struct {
				ItemID    string `json:"item_id"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(event.Data), &raw); err != nil {
				continue
			}
			if pc := pending[raw.ItemID]; pc != nil && raw.Arguments != "" {
				pc.arguments.Reset()
				pc.arguments.WriteString(raw.Arguments)
			}
		case "response.output_text.delta":
			var raw struct {
				OutputIndex int    `json:"output_index"`
				Delta       string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(event.Data), &raw); err != nil {
				continue
			}
			b := textByIndex[raw.OutputIndex]
			if b == nil {
				b = &strings.Builder{}
				textByIndex[raw.OutputIndex] = b
			}
			b.WriteString(raw.Delta)
		case "response.output_item.done":
			var raw struct {
				OutputIndex int                      `json:"output_index"`
				Item        openAIResponseOutputItem `json:"item"`
			}
			if err := json.Unmarshal([]byte(event.Data), &raw); err != nil {
				continue
			}
			// Also keep the item as raw JSON so unknown types
			// (reasoning, web_search_call, image_generation_call, …)
			// can be re-emitted verbatim in the synthesized SSE
			// stream when a sibling function_call triggers a rewrite.
			var rawItem struct {
				Item json.RawMessage `json:"item"`
			}
			_ = json.Unmarshal([]byte(event.Data), &rawItem)
			switch raw.Item.Type {
			case "message":
				txt := ""
				if b := textByIndex[raw.OutputIndex]; b != nil {
					txt = b.String()
					delete(textByIndex, raw.OutputIndex)
				}
				if txt != "" {
					frags = append(frags, assistantFragment{Text: txt})
				}
				orderedItems = append(orderedItems, orderedResponsesItem{
					isText:      true,
					outputIndex: raw.OutputIndex,
					text:        txt,
				})
			case "function_call":
				pc := pending[raw.Item.ID]
				if pc == nil {
					continue
				}
				if args := stringifyOpenAIArguments(raw.Item.Arguments); args != "" {
					pc.arguments.Reset()
					pc.arguments.WriteString(args)
				}
				originalArgs := pc.arguments.String()
				tu := ToolUse{
					ID:    pc.callID,
					Index: index,
					Name:  pc.name,
					Input: rawIfJSONOpenAI(originalArgs),
				}
				index++
				verdict := eval(tu)
				decisions = append(decisions, ToolUseDecisionRecord{
					ToolUse:          tu,
					Verdict:          verdict,
					ToolInputPreview: MakeToolInputPreview(tu.Input),
				})
				finalArgs := originalArgs
				fragArgs := tu.Input
				if !verdict.Allowed {
					anyBlocked = true
					txt := verdict.SubstituteWith
					if txt == "" && !verdict.SuppressSubstituteText {
						reason := verdict.Reason
						if reason == "" {
							reason = "blocked by policy"
						}
						txt = fmt.Sprintf("Tool '%s' was blocked by Clawvisor policy: %s", pc.name, reason)
					}
					if txt != "" {
						orderedItems = append(orderedItems, orderedResponsesItem{
							isText:      true,
							outputIndex: pc.outputIndex,
							text:        txt,
						})
					}
				} else {
					if len(verdict.RewriteInput) > 0 {
						finalArgs = string(verdict.RewriteInput)
						fragArgs = verdict.RewriteInput
						anyRewritten = true
					}
					orderedItems = append(orderedItems, orderedResponsesItem{
						outputIndex: pc.outputIndex,
						itemID:      pc.itemID,
						callID:      pc.callID,
						name:        pc.name,
						arguments:   finalArgs,
					})
				}
				frags = append(frags, assistantFragment{IsTool: true, ToolName: pc.name, ToolArgs: fragArgs})
				delete(pending, raw.Item.ID)
			case "custom_tool_call":
				tu, ok := toolUseFromOpenAICustomToolCall(raw.Item, index)
				if !ok {
					continue
				}
				index++
				verdict := eval(tu)
				decisions = append(decisions, ToolUseDecisionRecord{
					ToolUse:          tu,
					Verdict:          verdict,
					ToolInputPreview: MakeToolInputPreview(tu.Input),
				})
				if !verdict.Allowed {
					anyBlocked = true
					txt := verdict.SubstituteWith
					if txt == "" && !verdict.SuppressSubstituteText {
						reason := verdict.Reason
						if reason == "" {
							reason = "blocked by policy"
						}
						txt = fmt.Sprintf("Tool '%s' was blocked by Clawvisor policy: %s", tu.Name, reason)
					}
					if txt != "" {
						orderedItems = append(orderedItems, orderedResponsesItem{
							isText:      true,
							outputIndex: raw.OutputIndex,
							text:        txt,
						})
					}
				} else {
					orderedItems = append(orderedItems, orderedResponsesItem{
						isCustomToolCall: true,
						outputIndex:      raw.OutputIndex,
						itemID:           raw.Item.ID,
						callID:           raw.Item.CallID,
						name:             tu.Name,
						customInput:      customToolInputForReemit(raw.Item.Input, raw.Item.Arguments),
					})
				}
				frags = append(frags, assistantFragment{IsTool: true, ToolName: tu.Name, ToolArgs: tu.Input})
			default:
				// Unknown item type (reasoning, web_search_call,
				// image_generation_call, MCP tool calls, …). Preserve
				// the raw item so the synthesized rewrite SSE doesn't
				// silently drop it when a sibling function_call
				// triggers a rebuild.
				if len(rawItem.Item) > 0 {
					orderedItems = append(orderedItems, orderedResponsesItem{
						isPassThrough:  true,
						outputIndex:    raw.OutputIndex,
						passThroughRaw: append(json.RawMessage(nil), rawItem.Item...),
					})
				}
			}
		}
	}
	turn := assistantTurnFromFragments(frags, decisions)

	if anyBlocked || anyRewritten {
		// Emit a synthesized Responses-API SSE stream with the rewritten
		// function_call arguments substituted in. Other items pass
		// through verbatim.
		out := buildOpenAIResponsesMultiSSE(orderedItems)
		return RewriteResult{
			Body:          out,
			Decisions:     decisions,
			Rewritten:     true,
			AssistantTurn: turn,
		}, nil
	}
	return RewriteResult{Body: body, Decisions: decisions, AssistantTurn: turn}, nil
}

// buildOpenAIResponsesMultiSSE emits a Responses-API SSE stream
// containing the supplied output items in order. function_call items
// carry their (possibly rewritten) arguments; text items carry their
// accumulated content. Used by rewriteResponsesSSE when one or more
// function_call args were mutated on the rewrite path.
func buildOpenAIResponsesMultiSSE(items []orderedResponsesItem) []byte {
	var b strings.Builder
	b.WriteString(sseEventBlock("response.created", map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": "resp_clawvisor_rewrite", "status": "in_progress"},
	}))
	var outputItems []map[string]any
	var outputTexts []string
	for i, it := range items {
		outputIndex := it.outputIndex
		if it.isText {
			itemID := fmt.Sprintf("msg_clawvisor_%d", i)
			b.WriteString(sseEventBlock("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item":         map[string]any{"id": itemID, "type": "message", "role": "assistant", "status": "in_progress"},
			}))
			if it.text != "" {
				b.WriteString(sseEventBlock("response.output_text.delta", map[string]any{
					"type":          "response.output_text.delta",
					"item_id":       itemID,
					"output_index":  outputIndex,
					"content_index": 0,
					"delta":         it.text,
				}))
				b.WriteString(sseEventBlock("response.output_text.done", map[string]any{
					"type":          "response.output_text.done",
					"item_id":       itemID,
					"output_index":  outputIndex,
					"content_index": 0,
					"text":          it.text,
				}))
				outputTexts = append(outputTexts, it.text)
			}
			msgItem := map[string]any{
				"id":     itemID,
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]any{{
					"type": "output_text",
					"text": it.text,
				}},
			}
			outputItems = append(outputItems, msgItem)
			b.WriteString(sseEventBlock("response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item":         msgItem,
			}))
			continue
		}
		if it.isPassThrough && len(it.passThroughRaw) > 0 {
			// Decode the original item JSON so we can wrap it in the
			// expected output_item.added / output_item.done envelope
			// shape. Decoding into a map preserves arbitrary fields
			// the rewriter doesn't recognize.
			var passThrough map[string]any
			if err := json.Unmarshal(it.passThroughRaw, &passThrough); err != nil || passThrough == nil {
				continue
			}
			outputItems = append(outputItems, passThrough)
			b.WriteString(sseEventBlock("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item":         passThrough,
			}))
			b.WriteString(sseEventBlock("response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item":         passThrough,
			}))
			continue
		}
		if it.isCustomToolCall {
			itemID := it.itemID
			if itemID == "" {
				itemID = "ctc_" + it.callID
			}
			customItem := map[string]any{
				"id":      itemID,
				"type":    "custom_tool_call",
				"status":  "completed",
				"call_id": it.callID,
				"name":    it.name,
				"input":   it.customInput,
			}
			outputItems = append(outputItems, customItem)
			b.WriteString(sseEventBlock("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]any{
					"id":      itemID,
					"type":    "custom_tool_call",
					"status":  "completed",
					"call_id": it.callID,
					"name":    it.name,
					"input":   it.customInput,
				},
			}))
			b.WriteString(sseEventBlock("response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item":         customItem,
			}))
			continue
		}
		// function_call item.
		itemID := it.itemID
		if itemID == "" {
			itemID = "fc_" + it.callID
		}
		b.WriteString(sseEventBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": outputIndex,
			"item": map[string]any{
				"id":      itemID,
				"type":    "function_call",
				"status":  "in_progress",
				"call_id": it.callID,
				"name":    it.name,
			},
		}))
		b.WriteString(sseEventBlock("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      itemID,
			"output_index": outputIndex,
			"delta":        it.arguments,
		}))
		b.WriteString(sseEventBlock("response.function_call_arguments.done", map[string]any{
			"type":         "response.function_call_arguments.done",
			"item_id":      itemID,
			"output_index": outputIndex,
			"name":         it.name,
			"arguments":    it.arguments,
		}))
		fcItem := map[string]any{
			"id":        itemID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   it.callID,
			"name":      it.name,
			"arguments": it.arguments,
		}
		outputItems = append(outputItems, fcItem)
		b.WriteString(sseEventBlock("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": outputIndex,
			"item":         fcItem,
		}))
	}
	b.WriteString(sseEventBlock("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":          "resp_clawvisor_rewrite",
			"status":      "completed",
			"output":      outputItems,
			"output_text": strings.Join(outputTexts, "\n\n"),
		},
	}))
	b.WriteString("data: [DONE]\n\n")
	return []byte(b.String())
}

// orderedResponsesItem is the package-scoped type used by
// buildOpenAIResponsesMultiSSE. Mirrors the local orderedItem in
// rewriteResponsesSSE so the helper can be tested independently.
type orderedResponsesItem struct {
	isText           bool
	isCustomToolCall bool
	isPassThrough    bool
	outputIndex      int
	itemID           string
	callID           string
	name             string
	arguments        string
	text             string
	// customInput holds the value to emit for a custom_tool_call's
	// `input` field. Stored as `any` so a model-emitted string is
	// re-emitted as a JSON string (per the OpenAI Responses spec),
	// while any non-string shape the parser happened to accept also
	// round-trips through json.Marshal cleanly. Nil emits as `null`.
	customInput any
	// passThroughRaw is the raw `item` JSON for output_item types the
	// rewriter does not specifically know about (reasoning,
	// web_search_call, image_generation_call, MCP tool calls, …). The
	// synthesized rewrite SSE re-emits these verbatim.
	passThroughRaw json.RawMessage
}

type openAIChatCompletionsResponse struct {
	ID      string             `json:"id,omitempty"`
	Object  string             `json:"object,omitempty"`
	Model   string             `json:"model,omitempty"`
	Choices []openAIChatChoice `json:"choices,omitempty"`
}

type openAIChatChoice struct {
	Index        int               `json:"index"`
	Message      openAIChatMessage `json:"message"`
	Delta        openAIChatMessage `json:"delta"`
	FinishReason string            `json:"finish_reason,omitempty"`
}

type openAIChatMessage struct {
	Role      string               `json:"role,omitempty"`
	Content   any                  `json:"content,omitempty"`
	ToolCalls []openAIChatToolCall `json:"tool_calls,omitempty"`
}

type openAIChatToolCall struct {
	Index    int                `json:"index,omitempty"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function openAIChatFunction `json:"function"`
}

type openAIChatFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func (rw OpenAIResponseRewriter) rewriteChatCompletionsJSON(body []byte, eval ToolUseEvaluator) (RewriteResult, error) {
	var resp openAIChatCompletionsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return RewriteResult{Body: body}, nil
	}
	var (
		decisions    []ToolUseDecisionRecord
		frags        []assistantFragment
		anyBlocked   bool
		anyRewritten bool
		index        int
	)
	for ci, choice := range resp.Choices {
		var (
			allowedToolCalls []openAIChatToolCall
			blockedPrompts   []string
			choiceBlocked    bool
			choiceRewritten  bool
		)
		originalText := flattenOpenAIContentFromAny(choice.Message.Content)
		if originalText != "" {
			frags = append(frags, assistantFragment{Text: originalText})
		}
		for _, call := range choice.Message.ToolCalls {
			tu := ToolUse{
				ID:    firstNonEmpty(call.ID, fmt.Sprintf("chat-tool-%d", index)),
				Index: index,
				Name:  call.Function.Name,
				Input: rawIfJSONOpenAI(call.Function.Arguments),
			}
			index++
			verdict := eval(tu)
			decisions = append(decisions, ToolUseDecisionRecord{
				ToolUse:          tu,
				Verdict:          verdict,
				ToolInputPreview: MakeToolInputPreview(tu.Input),
			})
			finalArgs := tu.Input
			if !verdict.Allowed {
				anyBlocked = true
				choiceBlocked = true
				txt := verdict.SubstituteWith
				if txt == "" && !verdict.SuppressSubstituteText {
					reason := verdict.Reason
					if reason == "" {
						reason = "blocked by policy"
					}
					txt = fmt.Sprintf("Tool '%s' was blocked by Clawvisor policy: %s", call.Function.Name, reason)
				}
				if txt != "" {
					blockedPrompts = append(blockedPrompts, txt)
				}
			} else {
				if len(verdict.RewriteInput) > 0 {
					call.Function.Arguments = string(verdict.RewriteInput)
					finalArgs = verdict.RewriteInput
					anyRewritten = true
					choiceRewritten = true
				}
				allowedToolCalls = append(allowedToolCalls, call)
			}
			frags = append(frags, assistantFragment{IsTool: true, ToolName: call.Function.Name, ToolArgs: finalArgs})
		}
		if choiceBlocked || choiceRewritten {
			resp.Choices[ci].Message.ToolCalls = allowedToolCalls
			if len(blockedPrompts) > 0 {
				joinedPrompts := strings.Join(blockedPrompts, "\n\n")
				switch typed := choice.Message.Content.(type) {
				case []any:
					resp.Choices[ci].Message.Content = append(typed, map[string]any{
						"type": "text",
						"text": joinedPrompts,
					})
				case string:
					if typed != "" {
						resp.Choices[ci].Message.Content = typed + "\n\n" + joinedPrompts
					} else {
						resp.Choices[ci].Message.Content = joinedPrompts
					}
				default:
					resp.Choices[ci].Message.Content = joinedPrompts
				}
			}
			if len(allowedToolCalls) > 0 {
				resp.Choices[ci].FinishReason = "tool_calls"
			} else {
				resp.Choices[ci].FinishReason = "stop"
			}
		}
	}
	turn := assistantTurnFromFragments(frags, decisions)
	if anyBlocked || anyRewritten {
		rewritten, err := json.Marshal(resp)
		if err != nil {
			return RewriteResult{}, fmt.Errorf("openai chat: marshal rewritten response: %w", err)
		}
		return RewriteResult{Body: rewritten, Decisions: decisions, Rewritten: true, AssistantTurn: turn}, nil
	}
	return RewriteResult{Body: body, Decisions: decisions, AssistantTurn: turn}, nil
}

func (rw OpenAIResponseRewriter) rewriteChatCompletionsSSE(body []byte, eval ToolUseEvaluator) (RewriteResult, error) {
	lines := strings.Split(string(body), "\n")
	type pendingCall struct {
		id   string
		name string
		args strings.Builder
	}
	type pendingEvalBoundary struct {
		textPre      string
		pendingCalls map[int]*pendingCall
	}
	type choiceSSEState struct {
		index      int
		pending    map[int]*pendingCall
		text       strings.Builder
		boundaries []pendingEvalBoundary
	}
	type toolCallState struct {
		allowed        bool
		useRunes       bool
		arguments      string
		argumentsRunes []rune
		origTotalLen   int
		origReadLen    int
		writtenArgsLen int
	}
	type choiceEvaluation struct {
		blockedPrompts   string
		allowedToolCalls []orderedChatToolCall
		choiceBlocked    bool
		choiceRewritten  bool
		toolStates       map[int]*toolCallState
	}

	var choiceOrder []int
	choiceStates := map[int]*choiceSSEState{}
	getOrCreateState := func(ci int) *choiceSSEState {
		cs, ok := choiceStates[ci]
		if !ok {
			cs = &choiceSSEState{
				index:   ci,
				pending: map[int]*pendingCall{},
			}
			choiceStates[ci] = cs
			choiceOrder = append(choiceOrder, ci)
		}
		return cs
	}

	var streamID string

	// Pass 1: Parse all events to construct decisions and choiceEvaluations
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
			ID      string             `json:"id"`
			Choices []openAIChatChoice `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if event.ID != "" && streamID == "" {
			streamID = event.ID
		}
		for _, choice := range event.Choices {
			cs := getOrCreateState(choice.Index)
			if txt := flattenOpenAIContentFromAny(choice.Delta.Content); txt != "" {
				cs.text.WriteString(txt)
			}
			for _, tc := range choice.Delta.ToolCalls {
				pc := cs.pending[tc.Index]
				if pc == nil {
					pc = &pendingCall{}
					cs.pending[tc.Index] = pc
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
			if choice.FinishReason == "tool_calls" {
				cs.boundaries = append(cs.boundaries, pendingEvalBoundary{
					textPre:      cs.text.String(),
					pendingCalls: cs.pending,
				})
				cs.text.Reset()
				cs.pending = map[int]*pendingCall{}
			}
		}
	}

	for _, ci := range choiceOrder {
		cs := choiceStates[ci]
		if cs.text.Len() > 0 || len(cs.pending) > 0 {
			cs.boundaries = append(cs.boundaries, pendingEvalBoundary{
				textPre:      cs.text.String(),
				pendingCalls: cs.pending,
			})
			cs.text.Reset()
			cs.pending = map[int]*pendingCall{}
		}
	}

	choiceEvaluations := map[int][]choiceEvaluation{}
	var decisions []ToolUseDecisionRecord
	var frags []assistantFragment
	anyBlocked := false
	anyRewritten := false

	for _, ci := range choiceOrder {
		cs := choiceStates[ci]
		var evals []choiceEvaluation

		for _, boundary := range cs.boundaries {
			if boundary.textPre != "" {
				frags = append(frags, assistantFragment{Text: boundary.textPre})
			}
			if len(boundary.pendingCalls) == 0 {
				continue
			}

			toolCallIndexes := make([]int, 0, len(boundary.pendingCalls))
			for toolCallIndex := range boundary.pendingCalls {
				toolCallIndexes = append(toolCallIndexes, toolCallIndex)
			}
			sort.Ints(toolCallIndexes)

			var choiceBlockedPrompts strings.Builder
			var choiceOrderedChatCalls []orderedChatToolCall
			choiceBlocked := false
			choiceRewritten := false
			toolStates := map[int]*toolCallState{}

			for _, toolCallIndex := range toolCallIndexes {
				pc := boundary.pendingCalls[toolCallIndex]
				tu := ToolUse{
					ID:    pc.id,
					Index: toolCallIndex,
					Name:  pc.name,
					Input: rawIfJSONOpenAI(pc.args.String()),
				}
				verdict := eval(tu)
				decisions = append(decisions, ToolUseDecisionRecord{
					ToolUse:          tu,
					Verdict:          verdict,
					ToolInputPreview: MakeToolInputPreview(tu.Input),
				})
				finalArgs := pc.args.String()
				fragArgs := tu.Input
				if !verdict.Allowed {
					anyBlocked = true
					choiceBlocked = true
					txt := verdict.SubstituteWith
					if txt == "" && !verdict.SuppressSubstituteText {
						reason := verdict.Reason
						if reason == "" {
							reason = "blocked by policy"
						}
						txt = fmt.Sprintf("Tool '%s' was blocked by Clawvisor policy: %s", pc.name, reason)
					}
					if txt != "" {
						if choiceBlockedPrompts.Len() > 0 {
							choiceBlockedPrompts.WriteString("\n\n")
						}
						choiceBlockedPrompts.WriteString(txt)
					}
					origStr := pc.args.String()
					useRunes := utf8.ValidString(origStr)
					var origTotalLen int
					if useRunes {
						origTotalLen = len([]rune(origStr))
					} else {
						origTotalLen = len(origStr)
					}
					toolStates[toolCallIndex] = &toolCallState{
						allowed:      false,
						useRunes:     useRunes,
						origTotalLen: origTotalLen,
					}
				} else {
					if len(verdict.RewriteInput) > 0 {
						finalArgs = string(verdict.RewriteInput)
						fragArgs = verdict.RewriteInput
						anyRewritten = true
						choiceRewritten = true
					}
					choiceOrderedChatCalls = append(choiceOrderedChatCalls, orderedChatToolCall{
						index:     toolCallIndex,
						id:        pc.id,
						name:      pc.name,
						arguments: finalArgs,
					})
					origStr := pc.args.String()
					useRunes := utf8.ValidString(origStr) && utf8.ValidString(finalArgs)
					if useRunes {
						toolStates[toolCallIndex] = &toolCallState{
							allowed:        true,
							useRunes:       true,
							argumentsRunes: []rune(finalArgs),
							origTotalLen:   len([]rune(origStr)),
						}
					} else {
						toolStates[toolCallIndex] = &toolCallState{
							allowed:      true,
							useRunes:     false,
							arguments:    finalArgs,
							origTotalLen: len(origStr),
						}
					}
				}
				frags = append(frags, assistantFragment{IsTool: true, ToolName: pc.name, ToolArgs: fragArgs})
			}

			evals = append(evals, choiceEvaluation{
				blockedPrompts:   choiceBlockedPrompts.String(),
				allowedToolCalls: choiceOrderedChatCalls,
				choiceBlocked:    choiceBlocked,
				choiceRewritten:  choiceRewritten,
				toolStates:       toolStates,
			})
		}
		if len(evals) > 0 {
			choiceEvaluations[ci] = evals
		}
	}

	turn := assistantTurnFromFragments(frags, decisions)

	// Pass 2: Replay the original stream line-by-line, applying rewrites at the tool-call boundaries
	if anyBlocked || anyRewritten {
		var rewrittenBody strings.Builder
		replayTurn := map[int]int{}

		for _, originalLine := range lines {
			line := strings.TrimSpace(originalLine)
			if !strings.HasPrefix(line, "data:") {
				rewrittenBody.WriteString(originalLine + "\n")
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				rewrittenBody.WriteString("data:\n")
				continue
			}
			if payload == "[DONE]" {
				rewrittenBody.WriteString("data: [DONE]\n")
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				rewrittenBody.WriteString("data: " + payload + "\n")
				continue
			}
			choicesRaw, ok := event["choices"]
			if !ok {
				rewrittenBody.WriteString("data: " + payload + "\n")
				continue
			}
			eventChoices, ok := choicesRaw.([]any)
			if !ok {
				rewrittenBody.WriteString("data: " + payload + "\n")
				continue
			}

			for _, choiceRaw := range eventChoices {
				choice, ok := choiceRaw.(map[string]any)
				if !ok {
					continue
				}
				cIdxFloat, _ := choice["index"].(float64)
				cIdx := int(cIdxFloat)
				delta, _ := choice["delta"].(map[string]any)
				if delta == nil {
					delta = map[string]any{}
					choice["delta"] = delta
				}

				// Filter and rewrite incremental tool calls
				if toolCallsRaw, ok := delta["tool_calls"].([]any); ok {
					var newToolCalls []any
					for _, tcRaw := range toolCallsRaw {
						tc, ok := tcRaw.(map[string]any)
						if !ok {
							continue
						}
						tcIdxFloat, _ := tc["index"].(float64)
						tcIdx := int(tcIdxFloat)

						evals := choiceEvaluations[cIdx]
						rIdx := replayTurn[cIdx]
						if rIdx < len(evals) {
							eval := evals[rIdx]
							if tState := eval.toolStates[tcIdx]; tState != nil {
								if !tState.allowed {
									// Blocked: skip/omit from the stream
									continue
								}
								// Allowed: rewrite arguments delta proportionally (rune-based if UTF-8, byte-based fallback to avoid corruption of raw binary bytes)
								tcFunc, _ := tc["function"].(map[string]any)
								if tcFunc != nil {
									if origArgsDelta, ok := tcFunc["arguments"].(string); ok {
										if tState.useRunes {
											origArgsRunes := []rune(origArgsDelta)
											tState.origReadLen += len(origArgsRunes)
											var newDeltaRunes []rune
											if tState.origTotalLen == 0 {
												newDeltaRunes = tState.argumentsRunes
												tState.writtenArgsLen = len(tState.argumentsRunes)
											} else if tState.origReadLen >= tState.origTotalLen {
												newDeltaRunes = tState.argumentsRunes[tState.writtenArgsLen:]
												tState.writtenArgsLen = len(tState.argumentsRunes)
											} else {
												ratio := float64(len(origArgsRunes)) / float64(tState.origTotalLen)
												chunkSize := int(ratio * float64(len(tState.argumentsRunes)))
												if chunkSize < 1 && len(origArgsRunes) > 0 {
													chunkSize = 1
												}
												if tState.writtenArgsLen+chunkSize > len(tState.argumentsRunes) {
													chunkSize = len(tState.argumentsRunes) - tState.writtenArgsLen
												}
												newDeltaRunes = tState.argumentsRunes[tState.writtenArgsLen : tState.writtenArgsLen+chunkSize]
												tState.writtenArgsLen += chunkSize
											}
											tcFunc["arguments"] = string(newDeltaRunes)
										} else {
											tState.origReadLen += len(origArgsDelta)
											var newDelta string
											if tState.origTotalLen == 0 {
												newDelta = tState.arguments
												tState.writtenArgsLen = len(tState.arguments)
											} else if tState.origReadLen >= tState.origTotalLen {
												newDelta = tState.arguments[tState.writtenArgsLen:]
												tState.writtenArgsLen = len(tState.arguments)
											} else {
												ratio := float64(len(origArgsDelta)) / float64(tState.origTotalLen)
												chunkSize := int(ratio * float64(len(tState.arguments)))
												if chunkSize < 1 && len(origArgsDelta) > 0 {
													chunkSize = 1
												}
												if tState.writtenArgsLen+chunkSize > len(tState.arguments) {
													chunkSize = len(tState.arguments) - tState.writtenArgsLen
												}
												newDelta = tState.arguments[tState.writtenArgsLen : tState.writtenArgsLen+chunkSize]
												tState.writtenArgsLen += chunkSize
											}
											tcFunc["arguments"] = rawJSONString(newDelta)
										}
									}
								}
							}
						}
						newToolCalls = append(newToolCalls, tcRaw)
					}
					if len(newToolCalls) > 0 {
						delta["tool_calls"] = newToolCalls
					} else {
						delete(delta, "tool_calls")
					}
				}

				finishReason, _ := choice["finish_reason"].(string)

				if finishReason == "tool_calls" {
					evals := choiceEvaluations[cIdx]
					rIdx := replayTurn[cIdx]
					if rIdx < len(evals) {
						eval := evals[rIdx]
						replayTurn[cIdx]++

						if eval.blockedPrompts != "" {
							origContent, _ := delta["content"].(string)
							if origContent != "" {
								delta["content"] = origContent + "\n\n" + eval.blockedPrompts
							} else {
								delta["content"] = eval.blockedPrompts
							}
						}

						if len(eval.allowedToolCalls) > 0 {
							choice["finish_reason"] = "tool_calls"
						} else {
							choice["finish_reason"] = "stop"
						}
					} else {
						choice["finish_reason"] = "stop"
					}
				}
			}

			rewrittenPayload, err := json.Marshal(event)
			if err == nil {
				rewrittenBody.WriteString("data: " + string(rewrittenPayload) + "\n")
			} else {
				rewrittenBody.WriteString("data: " + payload + "\n")
			}
		}

		// Ensure the returned body ends with the standard trailing newlines
		bodyStr := rewrittenBody.String()
		if strings.HasSuffix(bodyStr, "\n") {
			bodyStr = strings.TrimSuffix(bodyStr, "\n")
		}
		return RewriteResult{Body: []byte(bodyStr + "\n"), Decisions: decisions, Rewritten: true, AssistantTurn: turn}, nil
	}

	return RewriteResult{Body: body, Decisions: decisions, AssistantTurn: turn}, nil
}

// orderedChatToolCall captures one tool_call in a Chat Completions
// stream, in the order it completed. Used to re-emit the assistant
// turn when one or more arguments were rewritten.
type orderedChatToolCall struct {
	index     int
	id        string
	name      string
	arguments string
}

func SynthOpenAIResponsesTextJSON(text string) []byte {
	out := openAIResponsesJSON{
		ID:     "resp_clawvisor_block",
		Object: "response",
		Output: []openAIResponseOutputItem{{
			ID:     "msg_clawvisor_block",
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []openAIResponseContent{{
				Type: "output_text",
				Text: text,
			}},
		}},
		OutputText: text,
	}
	body, _ := json.Marshal(out)
	return body
}

// SynthOpenAIResponsesFunctionCallsJSON builds a Responses-API JSON
// payload carrying N function_call items in `output`. Used by the
// coalesced-approval release path.
func SynthOpenAIResponsesFunctionCallsJSON(calls []SyntheticToolCall) []byte {
	items := make([]openAIResponseOutputItem, 0, len(calls))
	for _, call := range calls {
		input := call.Input
		if input == nil {
			input = map[string]any{}
		}
		args, _ := json.Marshal(input)
		items = append(items, openAIResponseOutputItem{
			ID:        "fc_" + call.ID,
			Type:      "function_call",
			Status:    "completed",
			CallID:    call.ID,
			Name:      call.Name,
			Arguments: string(args),
		})
	}
	out := openAIResponsesJSON{
		ID:     "resp_clawvisor_approve",
		Object: "response",
		Output: items,
	}
	body, _ := json.Marshal(out)
	return body
}

// SynthOpenAIResponsesFunctionCallsSSE is the SSE counterpart to
// SynthOpenAIResponsesFunctionCallsJSON: emits sequential output_item
// added/delta/done sequences for each function_call.
func SynthOpenAIResponsesFunctionCallsSSE(calls []SyntheticToolCall) []byte {
	var b strings.Builder
	b.WriteString(sseEventBlock("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_clawvisor_approve", "status": "in_progress"}}))
	for i, call := range calls {
		input := call.Input
		if input == nil {
			input = map[string]any{}
		}
		args, _ := json.Marshal(input)
		b.WriteString(sseEventBlock("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": i,
			"item":         map[string]any{"id": "fc_" + call.ID, "type": "function_call", "status": "in_progress", "call_id": call.ID, "name": call.Name},
		}))
		b.WriteString(sseEventBlock("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      "fc_" + call.ID,
			"output_index": i,
			"delta":        string(args),
		}))
		b.WriteString(sseEventBlock("response.function_call_arguments.done", map[string]any{
			"type":         "response.function_call_arguments.done",
			"item_id":      "fc_" + call.ID,
			"output_index": i,
			"name":         call.Name,
			"arguments":    string(args),
		}))
		b.WriteString(sseEventBlock("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": i,
			"item":         map[string]any{"id": "fc_" + call.ID, "type": "function_call", "status": "completed", "call_id": call.ID, "name": call.Name, "arguments": string(args)},
		}))
	}
	b.WriteString(sseEventBlock("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_clawvisor_approve", "status": "completed"}}))
	return []byte(b.String())
}

// SynthOpenAIChatToolCallsJSON builds a chat.completion JSON payload
// carrying N tool_calls on one assistant message. Used by the
// coalesced-approval release path.
func SynthOpenAIChatToolCallsJSON(calls []SyntheticToolCall) []byte {
	toolCalls := make([]openAIChatToolCall, 0, len(calls))
	for _, call := range calls {
		input := call.Input
		if input == nil {
			input = map[string]any{}
		}
		args, _ := json.Marshal(input)
		toolCalls = append(toolCalls, openAIChatToolCall{
			ID:   call.ID,
			Type: "function",
			Function: openAIChatFunction{
				Name:      call.Name,
				Arguments: string(args),
			},
		})
	}
	out := openAIChatCompletionsResponse{
		ID:     "chatcmpl_clawvisor_approve",
		Object: "chat.completion",
		Choices: []openAIChatChoice{{
			Index: 0,
			Message: openAIChatMessage{
				Role:      "assistant",
				ToolCalls: toolCalls,
			},
			FinishReason: "tool_calls",
		}},
	}
	body, _ := json.Marshal(out)
	return body
}

// SynthOpenAIChatToolCallsSSE is the SSE counterpart to
// SynthOpenAIChatToolCallsJSON: emits one tool_calls delta carrying all
// N entries (each with its own index in the array).
func SynthOpenAIChatToolCallsSSE(calls []SyntheticToolCall) []byte {
	var b strings.Builder
	b.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":      "chatcmpl_clawvisor_approve",
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	}))
	toolCalls := make([]map[string]any, 0, len(calls))
	for i, call := range calls {
		input := call.Input
		if input == nil {
			input = map[string]any{}
		}
		args, _ := json.Marshal(input)
		toolCalls = append(toolCalls, map[string]any{
			"index": i,
			"id":    call.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": string(args),
			},
		})
	}
	b.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":     "chatcmpl_clawvisor_approve",
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{"tool_calls": toolCalls},
			"finish_reason": nil,
		}},
	}))
	b.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":      "chatcmpl_clawvisor_approve",
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
	}))
	b.WriteString("data: [DONE]\n\n")
	return []byte(b.String())
}

func SynthOpenAIResponsesFunctionCallJSON(toolUseID, toolName string, toolInput map[string]any) []byte {
	args, _ := json.Marshal(toolInput)
	out := openAIResponsesJSON{
		ID:     "resp_clawvisor_approve",
		Object: "response",
		Output: []openAIResponseOutputItem{{
			ID:        "fc_" + toolUseID,
			Type:      "function_call",
			Status:    "completed",
			CallID:    toolUseID,
			Name:      toolName,
			Arguments: string(args),
		}},
	}
	body, _ := json.Marshal(out)
	return body
}

func SynthOpenAIChatTextJSON(text string) []byte {
	out := openAIChatCompletionsResponse{
		ID:     "chatcmpl_clawvisor_block",
		Object: "chat.completion",
		Choices: []openAIChatChoice{{
			Index: 0,
			Message: openAIChatMessage{
				Role:    "assistant",
				Content: text,
			},
			FinishReason: "stop",
		}},
	}
	body, _ := json.Marshal(out)
	return body
}

func SynthOpenAIChatToolCallJSON(toolUseID, toolName string, toolInput map[string]any) []byte {
	args, _ := json.Marshal(toolInput)
	out := openAIChatCompletionsResponse{
		ID:     "chatcmpl_clawvisor_approve",
		Object: "chat.completion",
		Choices: []openAIChatChoice{{
			Index: 0,
			Message: openAIChatMessage{
				Role: "assistant",
				ToolCalls: []openAIChatToolCall{{
					ID:   toolUseID,
					Type: "function",
					Function: openAIChatFunction{
						Name:      toolName,
						Arguments: string(args),
					},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	body, _ := json.Marshal(out)
	return body
}

func SynthOpenAIResponsesTextSSE(text string) []byte {
	return synthOpenAIResponsesTextSSE(text)
}

func SynthOpenAIResponsesFunctionCallSSE(toolUseID, toolName string, toolInput map[string]any) []byte {
	return synthOpenAIResponsesFunctionCallSSE(toolUseID, toolName, toolInput)
}

func SynthOpenAIChatTextSSE(text string) []byte {
	return synthOpenAIChatTextSSE(text)
}

func SynthOpenAIChatToolCallSSE(toolUseID, toolName string, toolInput map[string]any) []byte {
	return synthOpenAIChatToolCallSSE(toolUseID, toolName, toolInput)
}

func synthOpenAIResponsesTextSSE(text string) []byte {
	var b strings.Builder
	messageItem := map[string]any{
		"id":     "msg_clawvisor_block",
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []map[string]any{{
			"type": "output_text",
			"text": text,
		}},
	}
	b.WriteString(sseEventBlock("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_clawvisor_block", "status": "in_progress"}}))
	b.WriteString(sseEventBlock("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item":         map[string]any{"id": "msg_clawvisor_block", "type": "message", "role": "assistant", "status": "in_progress"},
	}))
	b.WriteString(sseEventBlock("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       "msg_clawvisor_block",
		"output_index":  0,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": ""},
	}))
	b.WriteString(sseEventBlock("response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       "msg_clawvisor_block",
		"output_index":  0,
		"content_index": 0,
		"delta":         text,
	}))
	b.WriteString(sseEventBlock("response.output_text.done", map[string]any{
		"type":          "response.output_text.done",
		"item_id":       "msg_clawvisor_block",
		"output_index":  0,
		"content_index": 0,
		"text":          text,
	}))
	b.WriteString(sseEventBlock("response.content_part.done", map[string]any{
		"type":          "response.content_part.done",
		"item_id":       "msg_clawvisor_block",
		"output_index":  0,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": text},
	}))
	b.WriteString(sseEventBlock("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": 0,
		"item":         messageItem,
	}))
	b.WriteString(sseEventBlock("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":          "resp_clawvisor_block",
			"status":      "completed",
			"output":      []map[string]any{messageItem},
			"output_text": text,
		},
	}))
	b.WriteString("data: [DONE]\n\n")
	return []byte(b.String())
}

func synthOpenAIResponsesFunctionCallSSE(toolUseID, toolName string, toolInput map[string]any) []byte {
	args, _ := json.Marshal(toolInput)
	var b strings.Builder
	b.WriteString(sseEventBlock("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_clawvisor_approve", "status": "in_progress"}}))
	b.WriteString(sseEventBlock("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item":         map[string]any{"id": "fc_" + toolUseID, "type": "function_call", "status": "in_progress", "call_id": toolUseID, "name": toolName},
	}))
	b.WriteString(sseEventBlock("response.function_call_arguments.delta", map[string]any{
		"type":         "response.function_call_arguments.delta",
		"item_id":      "fc_" + toolUseID,
		"output_index": 0,
		"delta":        string(args),
	}))
	b.WriteString(sseEventBlock("response.function_call_arguments.done", map[string]any{
		"type":         "response.function_call_arguments.done",
		"item_id":      "fc_" + toolUseID,
		"output_index": 0,
		"name":         toolName,
		"arguments":    string(args),
	}))
	b.WriteString(sseEventBlock("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": 0,
		"item":         map[string]any{"id": "fc_" + toolUseID, "type": "function_call", "status": "completed", "call_id": toolUseID, "name": toolName, "arguments": string(args)},
	}))
	b.WriteString(sseEventBlock("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_clawvisor_approve", "status": "completed"}}))
	return []byte(b.String())
}

func synthOpenAIChatTextSSE(text string) []byte {
	var b strings.Builder
	b.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":      "chatcmpl_clawvisor_block",
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	}))
	b.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":      "chatcmpl_clawvisor_block",
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
	}))
	b.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":      "chatcmpl_clawvisor_block",
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}))
	b.WriteString("data: [DONE]\n\n")
	return []byte(b.String())
}

func synthOpenAIChatToolCallSSE(toolUseID, toolName string, toolInput map[string]any) []byte {
	args, _ := json.Marshal(toolInput)
	var b strings.Builder
	b.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":      "chatcmpl_clawvisor_approve",
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	}))
	b.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":     "chatcmpl_clawvisor_approve",
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []map[string]any{{
					"index": 0,
					"id":    toolUseID,
					"type":  "function",
					"function": map[string]any{
						"name":      toolName,
						"arguments": string(args),
					},
				}},
			},
			"finish_reason": nil,
		}},
	}))
	b.WriteString(chatCompletionSSEBlock(map[string]any{
		"id":      "chatcmpl_clawvisor_approve",
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
	}))
	b.WriteString("data: [DONE]\n\n")
	return []byte(b.String())
}

func sseEventBlock(event string, data any) string {
	raw, _ := json.Marshal(data)
	return "event: " + event + "\ndata: " + string(raw) + "\n\n"
}

func chatCompletionSSEBlock(data any) string {
	raw, _ := json.Marshal(data)
	return "data: " + string(raw) + "\n\n"
}

func isOpenAIResponsesBody(body []byte) bool {
	return bytes.Contains(body, []byte(`"output"`)) || bytes.Contains(body, []byte(`response.output_item.added`))
}

func isOpenAIChatCompletionsEndpointFromBody(contentType string, body []byte) bool {
	if isSSE(contentType) {
		return !bytes.Contains(body, []byte(`response.output_item.added`))
	}
	return bytes.Contains(body, []byte(`"choices"`))
}

func stringifyOpenAIArguments(v any) string {
	switch typed := v.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.RawMessage:
		return unwrapOpenAIArguments(typed)
	case []byte:
		return unwrapOpenAIArguments(json.RawMessage(typed))
	default:
		if v == nil {
			return ""
		}
		raw, _ := json.Marshal(v)
		return unwrapOpenAIArguments(raw)
	}
}

func flattenOpenAIContentFromAny(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case nil:
		return ""
	default:
		raw, _ := json.Marshal(typed)
		return flattenOpenAIContent(raw)
	}
}

func rawIfJSONOpenAI(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" || !json.Valid([]byte(args)) {
		return nil
	}
	return json.RawMessage(args)
}

func toolUseFromOpenAICustomToolCall(item openAIResponseOutputItem, index int) (ToolUse, bool) {
	name := strings.TrimSpace(item.Name)
	if name == "" {
		return ToolUse{}, false
	}
	input := stringifyOpenAIArguments(item.Input)
	if input == "" {
		input = stringifyOpenAIArguments(item.Arguments)
	}
	return ToolUse{
		ID:    firstNonEmpty(item.CallID, item.ID),
		Index: index,
		Name:  name,
		Input: rawOpenAICustomToolInput(input),
	}, true
}

// customToolInputForReemit returns the value to place in the
// `input` field of a synthesized custom_tool_call event. The OpenAI
// Responses API documents `input` as a string, so we preserve the
// model's original wire value and let json.Marshal escape it
// correctly. Mirrors toolUseFromOpenAICustomToolCall: prefer
// `item.Input`, fall back to `item.Arguments`. If both are empty,
// returns nil (which marshals as JSON null).
func customToolInputForReemit(input, arguments any) any {
	if v := customToolValueIfNonEmpty(input); v != nil {
		return v
	}
	if v := customToolValueIfNonEmpty(arguments); v != nil {
		return v
	}
	return nil
}

// customToolValueIfNonEmpty returns v when it carries a non-empty
// payload, otherwise nil. Strings are empty when whitespace-only;
// other types are kept as-is when non-nil.
func customToolValueIfNonEmpty(v any) any {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return s
	}
	return v
}

func rawOpenAICustomToolInput(input string) json.RawMessage {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}
	if json.Valid([]byte(input)) {
		return json.RawMessage(input)
	}
	raw, _ := json.Marshal(map[string]string{"input": input})
	return raw
}

func rawOpenAICustomToolInputFromAny(input any) json.RawMessage {
	switch v := input.(type) {
	case nil:
		return nil
	case string:
		return rawOpenAICustomToolInput(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return raw
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (rw OpenAIResponseRewriter) StreamRewrite(ctx context.Context, r io.Reader, w io.Writer, onToolUse func(ToolUse)) (StreamingRewriteResult, error) {
	br := bufio.NewReader(r)
	prefix, isResponses, err := sniffOpenAIStreamFormat(br)
	if err != nil {
		return StreamingRewriteResult{}, err
	}
	stream := io.MultiReader(bytes.NewReader(prefix), br)
	// OpenAI's chat/responses formats finalize each tool_use only at
	// end-of-stream (chat) or item-completed events (responses). Both
	// sub-paths build the full ToolUses slice during scanning; we fire
	// onToolUse after each entry lands so the orchestrator sees them
	// through the same incremental surface as Anthropic.
	deliver := func(result StreamingRewriteResult, err error) (StreamingRewriteResult, error) {
		if err == nil && onToolUse != nil {
			for _, tu := range result.ToolUses {
				onToolUse(tu)
			}
		}
		return result, err
	}

	if isResponses {
		return deliver(rw.streamRewriteResponses(ctx, stream, w))
	}
	return deliver(rw.streamRewriteChatCompletions(ctx, stream, w))
}

func sniffOpenAIStreamFormat(br *bufio.Reader) ([]byte, bool, error) {
	var prefix bytes.Buffer
	for prefix.Len() < 64<<10 {
		line, err := br.ReadString('\n')
		if line != "" {
			prefix.WriteString(line)
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "event:") {
				event := strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
				if strings.HasPrefix(event, "response.") {
					return prefix.Bytes(), true, nil
				}
			}
			if strings.HasPrefix(trimmed, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if data != "[DONE]" && (strings.Contains(data, `"type":"response.`) || strings.Contains(data, `"type": "response.`) || strings.Contains(data, `"object":"response"`) || strings.Contains(data, `"object": "response"`)) {
					return prefix.Bytes(), true, nil
				}
				return prefix.Bytes(), false, nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return prefix.Bytes(), bytes.Contains(prefix.Bytes(), []byte("response.")), nil
			}
			return prefix.Bytes(), false, err
		}
	}
	return prefix.Bytes(), bytes.Contains(prefix.Bytes(), []byte("response.")), nil
}

func (rw OpenAIResponseRewriter) streamRewriteChatCompletions(ctx context.Context, r io.Reader, w io.Writer) (StreamingRewriteResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)

	type pendingCall struct {
		id   string
		name string
		args strings.Builder
	}
	pending := map[int]*pendingCall{}
	var streamID string
	var msgModel string
	var text strings.Builder
	var frags []assistantFragment

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return StreamingRewriteResult{StreamID: streamID, Model: msgModel, StreamFormat: "openai_chat"}, err
		}
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if trimmed == "data: [DONE]" {
			if len(pending) == 0 {
				if _, err := fmt.Fprintln(w, line); err != nil {
					return StreamingRewriteResult{StreamID: streamID, Model: msgModel, StreamFormat: "openai_chat"}, err
				}
			}
			continue
		}
		if !strings.HasPrefix(trimmed, "data:") {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return StreamingRewriteResult{StreamID: streamID, Model: msgModel, StreamFormat: "openai_chat"}, err
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		var event struct {
			ID      string             `json:"id"`
			Model   string             `json:"model"`
			Choices []openAIChatChoice `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return StreamingRewriteResult{StreamID: streamID, Model: msgModel, StreamFormat: "openai_chat"}, err
			}
			continue
		}
		if event.ID != "" && streamID == "" {
			streamID = event.ID
		}
		if event.Model != "" && msgModel == "" {
			msgModel = event.Model
		}

		hasToolCalls := false
		var contentOnlyChoices []map[string]any
		for _, choice := range event.Choices {
			if len(choice.Delta.ToolCalls) > 0 {
				hasToolCalls = true
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
			if choice.Delta.Content != nil || flattenOpenAIContentFromAny(choice.Delta.Content) != "" {
				if txt := flattenOpenAIContentFromAny(choice.Delta.Content); txt != "" {
					text.WriteString(txt)
					contentOnlyChoices = append(contentOnlyChoices, map[string]any{
						"index": choice.Index,
						"delta": map[string]any{
							"content": choice.Delta.Content,
						},
						"finish_reason": nil,
					})
				}
			}
		}

		if hasToolCalls {
			if len(contentOnlyChoices) > 0 {
				reemitPayload := map[string]any{
					"id":      event.ID,
					"object":  "chat.completion.chunk",
					"model":   event.Model,
					"choices": contentOnlyChoices,
				}
				raw, err := json.Marshal(reemitPayload)
				if err != nil {
					return StreamingRewriteResult{StreamID: streamID, Model: msgModel, StreamFormat: "openai_chat"}, err
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", string(raw)); err != nil {
					return StreamingRewriteResult{StreamID: streamID, Model: msgModel, StreamFormat: "openai_chat"}, err
				}
			}
			continue
		}

		isToolCallFinish := false
		for _, choice := range event.Choices {
			if choice.FinishReason == "tool_calls" {
				isToolCallFinish = true
			}
		}
		if isToolCallFinish {
			continue
		}

		if _, err := fmt.Fprintln(w, line); err != nil {
			return StreamingRewriteResult{StreamID: streamID, Model: msgModel, StreamFormat: "openai_chat"}, err
		}
	}

	if err := scanner.Err(); err != nil {
		return StreamingRewriteResult{StreamID: streamID, Model: msgModel, StreamFormat: "openai_chat"}, err
	}

	if text.Len() > 0 {
		frags = append(frags, assistantFragment{Text: text.String()})
	}

	toolCallIndexes := make([]int, 0, len(pending))
	for toolCallIndex := range pending {
		toolCallIndexes = append(toolCallIndexes, toolCallIndex)
	}
	sort.Ints(toolCallIndexes)

	var tus []ToolUse
	for _, toolCallIndex := range toolCallIndexes {
		pc := pending[toolCallIndex]
		tus = append(tus, ToolUse{
			ID:    pc.id,
			Index: toolCallIndex,
			Name:  pc.name,
			Input: rawIfJSONOpenAI(pc.args.String()),
		})
		frags = append(frags, assistantFragment{
			IsTool:   true,
			ToolName: pc.name,
			ToolArgs: rawIfJSONOpenAI(pc.args.String()),
		})
	}

	turn := &Turn{
		Role:    RoleAssistant,
		Content: formatAssistantContent(frags),
	}

	return StreamingRewriteResult{
		ToolUses:      tus,
		AssistantTurn: turn,
		StreamID:      streamID,
		Model:         msgModel,
		StreamFormat:  "openai_chat",
	}, nil
}

func (rw OpenAIResponseRewriter) streamRewriteResponses(ctx context.Context, r io.Reader, w io.Writer) (StreamingRewriteResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)

	type pendingCall struct {
		itemID      string
		callID      string
		name        string
		outputIndex int
		arguments   strings.Builder
	}
	pending := map[string]*pendingCall{}
	textByIndex := map[int]*strings.Builder{}
	var orderedItems []orderedResponsesItem
	var lastEvent string
	var dataLns []string
	withheldTool := false
	streamID := "resp_clawvisor_rewrite"

	flushEvent := func() error {
		if len(dataLns) == 0 {
			lastEvent = ""
			return nil
		}
		data := strings.Join(dataLns, "\n")
		event := lastEvent
		lastEvent = ""
		dataLns = dataLns[:0]

		if data == "[DONE]" {
			return nil
		}

		switch event {
		case "response.created":
			var raw struct {
				Response struct {
					ID string `json:"id"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &raw); err == nil && raw.Response.ID != "" {
				streamID = raw.Response.ID
			}
			return writeSSE(w, event, data)

		case "response.output_item.added":
			var raw struct {
				OutputIndex int                      `json:"output_index"`
				Item        openAIResponseOutputItem `json:"item"`
			}
			if err := json.Unmarshal([]byte(data), &raw); err != nil {
				return writeSSE(w, event, data)
			}
			switch raw.Item.Type {
			case "message":
				if _, ok := textByIndex[raw.OutputIndex]; !ok {
					textByIndex[raw.OutputIndex] = &strings.Builder{}
				}
				return writeSSE(w, event, data)
			case "function_call":
				withheldTool = true
				pc := &pendingCall{
					itemID:      raw.Item.ID,
					callID:      firstNonEmpty(raw.Item.CallID, raw.Item.ID),
					name:        raw.Item.Name,
					outputIndex: raw.OutputIndex,
				}
				if args := stringifyOpenAIArguments(raw.Item.Arguments); args != "" {
					pc.arguments.WriteString(args)
				}
				pending[raw.Item.ID] = pc
				return nil
			case "custom_tool_call":
				withheldTool = true
				return nil
			}
			return writeSSE(w, event, data)

		case "response.function_call_arguments.delta":
			var raw struct {
				ItemID string `json:"item_id"`
				Delta  string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &raw); err != nil {
				return writeSSE(w, event, data)
			}
			if pc := pending[raw.ItemID]; pc != nil {
				pc.arguments.WriteString(raw.Delta)
			}
			return nil

		case "response.function_call_arguments.done":
			var raw struct {
				ItemID    string `json:"item_id"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(data), &raw); err != nil {
				return writeSSE(w, event, data)
			}
			if pc := pending[raw.ItemID]; pc != nil && raw.Arguments != "" {
				pc.arguments.Reset()
				pc.arguments.WriteString(raw.Arguments)
			}
			return nil

		case "response.output_text.delta":
			var raw struct {
				OutputIndex int    `json:"output_index"`
				Delta       string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &raw); err != nil {
				return writeSSE(w, event, data)
			}
			b := textByIndex[raw.OutputIndex]
			if b == nil {
				b = &strings.Builder{}
				textByIndex[raw.OutputIndex] = b
			}
			b.WriteString(raw.Delta)
			return writeSSE(w, event, data)

		case "response.output_item.done":
			var raw struct {
				OutputIndex int                      `json:"output_index"`
				Item        openAIResponseOutputItem `json:"item"`
			}
			if err := json.Unmarshal([]byte(data), &raw); err != nil {
				return writeSSE(w, event, data)
			}
			var rawItem struct {
				Item json.RawMessage `json:"item"`
			}
			_ = json.Unmarshal([]byte(data), &rawItem)

			switch raw.Item.Type {
			case "message":
				txt := ""
				if b := textByIndex[raw.OutputIndex]; b != nil {
					txt = b.String()
					delete(textByIndex, raw.OutputIndex)
				}
				orderedItems = append(orderedItems, orderedResponsesItem{
					isText:      true,
					outputIndex: raw.OutputIndex,
					text:        txt,
				})
				return writeSSE(w, event, data)
			case "function_call":
				pc := pending[raw.Item.ID]
				if pc == nil {
					return nil
				}
				if args := stringifyOpenAIArguments(raw.Item.Arguments); args != "" {
					pc.arguments.Reset()
					pc.arguments.WriteString(args)
				}
				orderedItems = append(orderedItems, orderedResponsesItem{
					outputIndex: pc.outputIndex,
					itemID:      pc.itemID,
					callID:      pc.callID,
					name:        pc.name,
					arguments:   pc.arguments.String(),
				})
				return nil
			case "custom_tool_call":
				withheldTool = true
				tu, ok := toolUseFromOpenAICustomToolCall(raw.Item, 0)
				if !ok {
					return writeSSE(w, event, data)
				}
				orderedItems = append(orderedItems, orderedResponsesItem{
					isCustomToolCall: true,
					outputIndex:      raw.OutputIndex,
					itemID:           raw.Item.ID,
					callID:           raw.Item.CallID,
					name:             tu.Name,
					customInput:      customToolInputForReemit(raw.Item.Input, raw.Item.Arguments),
				})
				return nil
			default:
				if len(rawItem.Item) > 0 {
					orderedItems = append(orderedItems, orderedResponsesItem{
						isPassThrough:  true,
						outputIndex:    raw.OutputIndex,
						passThroughRaw: append(json.RawMessage(nil), rawItem.Item...),
					})
				}
				return writeSSE(w, event, data)
			}

		case "response.completed":
			if !withheldTool {
				return writeSSE(w, event, data)
			}
			return nil
		}
		return writeSSE(w, event, data)
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return StreamingRewriteResult{StreamID: streamID, StreamFormat: "openai_responses"}, err
		}
		line := scanner.Text()
		trimmed := strings.TrimRight(line, "\r")
		if trimmed == "" {
			if err := flushEvent(); err != nil {
				return StreamingRewriteResult{StreamID: streamID, StreamFormat: "openai_responses"}, err
			}
			continue
		}
		if strings.HasPrefix(trimmed, ":") {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return StreamingRewriteResult{StreamID: streamID, StreamFormat: "openai_responses"}, err
			}
			continue
		}
		if strings.HasPrefix(trimmed, "event:") {
			lastEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			continue
		}
		if strings.HasPrefix(trimmed, "data:") {
			dataLns = append(dataLns, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return StreamingRewriteResult{StreamID: streamID, StreamFormat: "openai_responses"}, err
	}
	if err := flushEvent(); err != nil {
		return StreamingRewriteResult{StreamID: streamID, StreamFormat: "openai_responses"}, err
	}

	var tus []ToolUse
	var frags []assistantFragment
	index := 0
	nextOutputIndex := 0
	for _, item := range orderedItems {
		if item.outputIndex >= nextOutputIndex {
			nextOutputIndex = item.outputIndex + 1
		}
		if item.isText {
			frags = append(frags, assistantFragment{Text: item.text})
			continue
		}
		if item.isPassThrough {
			continue
		}
		if item.isCustomToolCall {
			inputRaw := rawOpenAICustomToolInputFromAny(item.customInput)
			tus = append(tus, ToolUse{
				ID:    firstNonEmpty(item.callID, item.itemID),
				Index: index,
				Name:  item.name,
				Input: inputRaw,
			})
			index++
			frags = append(frags, assistantFragment{
				IsTool:   true,
				ToolName: item.name,
				ToolArgs: inputRaw,
			})
			continue
		}
		pc := pending[item.itemID]
		args := item.arguments
		if pc != nil && pc.arguments.Len() > 0 {
			args = pc.arguments.String()
		}
		tus = append(tus, ToolUse{
			ID:    item.callID,
			Index: index,
			Name:  item.name,
			Input: rawIfJSONOpenAI(args),
		})
		index++
		frags = append(frags, assistantFragment{
			IsTool:   true,
			ToolName: item.name,
			ToolArgs: rawIfJSONOpenAI(args),
		})
	}

	turn := &Turn{
		Role:    RoleAssistant,
		Content: formatAssistantContent(frags),
	}

	return StreamingRewriteResult{
		ToolUses:              tus,
		AssistantTurn:         turn,
		StreamID:              streamID,
		StreamFormat:          "openai_responses",
		NextOpenAIOutputIndex: nextOutputIndex,
	}, nil
}

func writeSSE(w io.Writer, event string, data string) error {
	var err error
	if event != "" {
		_, err = fmt.Fprintf(w, "event: %s\n", event)
		if err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

type rawJSONString string

func (r rawJSONString) MarshalJSON() ([]byte, error) {
	res := make([]byte, 0, len(r)+2)
	res = append(res, '"')
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch c {
		case '"':
			res = append(res, '\\', '"')
		case '\\':
			res = append(res, '\\', '\\')
		case '\n':
			res = append(res, '\\', 'n')
		case '\r':
			res = append(res, '\\', 'r')
		case '\t':
			res = append(res, '\\', 't')
		default:
			if c < 0x20 {
				res = append(res, []byte(fmt.Sprintf("\\u%04x", c))...)
			} else {
				res = append(res, c)
			}
		}
	}
	res = append(res, '"')
	return res, nil
}
