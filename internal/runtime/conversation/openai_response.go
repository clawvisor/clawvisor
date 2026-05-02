package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
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
		if isSSE(contentType) {
			if bytes.Contains(body, []byte("response.output_item.added")) || bytes.Contains(body, []byte("response.function_call_arguments")) {
				return rw.rewriteResponses(body, contentType, eval)
			}
			return rw.rewriteChatCompletions(body, contentType, eval)
		}
		return RewriteResult{Body: body}, nil
	}
}

func (rw OpenAIResponseRewriter) rewriteResponses(body []byte, contentType string, eval ToolUseEvaluator) (RewriteResult, error) {
	if isSSE(contentType) {
		return rw.rewriteResponsesSSE(body, eval)
	}
	return rw.rewriteResponsesJSON(body, eval)
}

func (rw OpenAIResponseRewriter) rewriteChatCompletions(body []byte, contentType string, eval ToolUseEvaluator) (RewriteResult, error) {
	if isSSE(contentType) {
		return rw.rewriteChatCompletionsSSE(body, eval)
	}
	return rw.rewriteChatCompletionsJSON(body, eval)
}

type openAIResponsesJSON struct {
	ID        string                     `json:"id,omitempty"`
	Object    string                     `json:"object,omitempty"`
	Model     string                     `json:"model,omitempty"`
	Output    []openAIResponseOutputItem `json:"output,omitempty"`
	OutputText string                    `json:"output_text,omitempty"`
}

type openAIResponseOutputItem struct {
	ID        string                    `json:"id,omitempty"`
	Type      string                    `json:"type"`
	Role      string                    `json:"role,omitempty"`
	Status    string                    `json:"status,omitempty"`
	CallID    string                    `json:"call_id,omitempty"`
	Name      string                    `json:"name,omitempty"`
	Arguments any                       `json:"arguments,omitempty"`
	Content   []openAIResponseContent   `json:"content,omitempty"`
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
		decisions  []ToolUseDecisionRecord
		frags      []assistantFragment
		anyBlocked bool
		index      int
	)
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if (part.Type == "output_text" || part.Type == "text") && part.Text != "" {
					frags = append(frags, assistantFragment{Text: part.Text})
				}
			}
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
			if !verdict.Allowed {
				anyBlocked = true
			}
			frags = append(frags, assistantFragment{IsTool: true, ToolName: item.Name, ToolArgs: tu.Input})
		}
	}
	turn := assistantTurnFromFragments(frags, decisions)
	if !anyBlocked {
		return RewriteResult{Body: body, Decisions: decisions, AssistantTurn: turn}, nil
	}
	out := openAIResponsesJSON{
		ID:     resp.ID,
		Object: firstNonEmpty(resp.Object, "response"),
		Model:  resp.Model,
		Output: []openAIResponseOutputItem{{
			ID:     "msg_clawvisor_block",
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []openAIResponseContent{{
				Type: "output_text",
				Text: blockedReasonText(decisions),
			}},
		}},
		OutputText: blockedReasonText(decisions),
	}
	rewritten, err := json.Marshal(out)
	if err != nil {
		return RewriteResult{}, fmt.Errorf("openai responses: marshal rewritten response: %w", err)
	}
	return RewriteResult{Body: rewritten, Decisions: decisions, Rewritten: true, AssistantTurn: turn}, nil
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
	anyBlocked := false
	index := 0
	for _, event := range events {
		switch event.Event {
		case "response.output_item.added":
			var raw struct {
				OutputIndex int                    `json:"output_index"`
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
				OutputIndex int                    `json:"output_index"`
				Item        openAIResponseOutputItem `json:"item"`
			}
			if err := json.Unmarshal([]byte(event.Data), &raw); err != nil {
				continue
			}
			switch raw.Item.Type {
			case "message":
				if b := textByIndex[raw.OutputIndex]; b != nil && b.Len() > 0 {
					frags = append(frags, assistantFragment{Text: b.String()})
					delete(textByIndex, raw.OutputIndex)
				}
			case "function_call":
				pc := pending[raw.Item.ID]
				if pc == nil {
					continue
				}
				if args := stringifyOpenAIArguments(raw.Item.Arguments); args != "" {
					pc.arguments.Reset()
					pc.arguments.WriteString(args)
				}
				tu := ToolUse{
					ID:    pc.callID,
					Index: index,
					Name:  pc.name,
					Input: rawIfJSONOpenAI(pc.arguments.String()),
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
				}
				frags = append(frags, assistantFragment{IsTool: true, ToolName: pc.name, ToolArgs: tu.Input})
				delete(pending, raw.Item.ID)
			}
		}
	}
	turn := assistantTurnFromFragments(frags, decisions)
	if !anyBlocked {
		return RewriteResult{Body: body, Decisions: decisions, AssistantTurn: turn}, nil
	}
	return RewriteResult{
		Body:          synthOpenAIResponsesTextSSE(blockedReasonText(decisions)),
		Decisions:     decisions,
		Rewritten:     true,
		AssistantTurn: turn,
	}, nil
}

type openAIChatCompletionsResponse struct {
	ID      string                     `json:"id,omitempty"`
	Object  string                     `json:"object,omitempty"`
	Model   string                     `json:"model,omitempty"`
	Choices []openAIChatChoice         `json:"choices,omitempty"`
}

type openAIChatChoice struct {
	Index        int                 `json:"index"`
	Message      openAIChatMessage   `json:"message"`
	Delta        openAIChatMessage   `json:"delta"`
	FinishReason string              `json:"finish_reason,omitempty"`
}

type openAIChatMessage struct {
	Role      string               `json:"role,omitempty"`
	Content   any                  `json:"content,omitempty"`
	ToolCalls []openAIChatToolCall `json:"tool_calls,omitempty"`
}

type openAIChatToolCall struct {
	Index    int                 `json:"index,omitempty"`
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function openAIChatFunction  `json:"function"`
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
		decisions  []ToolUseDecisionRecord
		frags      []assistantFragment
		anyBlocked bool
		index      int
	)
	for _, choice := range resp.Choices {
		if text := flattenOpenAIContentFromAny(choice.Message.Content); text != "" {
			frags = append(frags, assistantFragment{Text: text})
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
			if !verdict.Allowed {
				anyBlocked = true
			}
			frags = append(frags, assistantFragment{IsTool: true, ToolName: call.Function.Name, ToolArgs: tu.Input})
		}
	}
	turn := assistantTurnFromFragments(frags, decisions)
	if !anyBlocked {
		return RewriteResult{Body: body, Decisions: decisions, AssistantTurn: turn}, nil
	}
	out := openAIChatCompletionsResponse{
		ID:     resp.ID,
		Object: firstNonEmpty(resp.Object, "chat.completion"),
		Model:  resp.Model,
		Choices: []openAIChatChoice{{
			Index: 0,
			Message: openAIChatMessage{
				Role:    "assistant",
				Content: blockedReasonText(decisions),
			},
			FinishReason: "stop",
		}},
	}
	rewritten, err := json.Marshal(out)
	if err != nil {
		return RewriteResult{}, fmt.Errorf("openai chat: marshal rewritten response: %w", err)
	}
	return RewriteResult{Body: rewritten, Decisions: decisions, Rewritten: true, AssistantTurn: turn}, nil
}

func (rw OpenAIResponseRewriter) rewriteChatCompletionsSSE(body []byte, eval ToolUseEvaluator) (RewriteResult, error) {
	lines := strings.Split(string(body), "\n")
	type pendingCall struct {
		id   string
		name string
		args strings.Builder
	}
	pending := map[int]*pendingCall{}
	var decisions []ToolUseDecisionRecord
	var frags []assistantFragment
	anyBlocked := false
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
		for _, choice := range event.Choices {
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
				if choice.FinishReason == "tool_calls" {
					if text.Len() > 0 {
						frags = append(frags, assistantFragment{Text: text.String()})
						text.Reset()
					}
					toolCallIndexes := make([]int, 0, len(pending))
					for toolCallIndex := range pending {
						toolCallIndexes = append(toolCallIndexes, toolCallIndex)
					}
					sort.Ints(toolCallIndexes)
					for _, toolCallIndex := range toolCallIndexes {
						pc := pending[toolCallIndex]
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
					if !verdict.Allowed {
						anyBlocked = true
					}
					frags = append(frags, assistantFragment{IsTool: true, ToolName: pc.name, ToolArgs: tu.Input})
				}
				pending = map[int]*pendingCall{}
			}
		}
	}
	if text.Len() > 0 {
		frags = append(frags, assistantFragment{Text: text.String()})
	}
	turn := assistantTurnFromFragments(frags, decisions)
	if !anyBlocked {
		return RewriteResult{Body: body, Decisions: decisions, AssistantTurn: turn}, nil
	}
	return RewriteResult{
		Body:          synthOpenAIChatTextSSE(blockedReasonText(decisions)),
		Decisions:     decisions,
		Rewritten:     true,
		AssistantTurn: turn,
	}, nil
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
		"item":         map[string]any{"id": "msg_clawvisor_block", "type": "message", "role": "assistant", "status": "completed"},
	}))
	b.WriteString(sseEventBlock("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_clawvisor_block", "status": "completed"}}))
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
