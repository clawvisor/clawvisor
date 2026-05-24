package conversation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type AnthropicResponseRewriter struct{}

func (AnthropicResponseRewriter) Name() Provider { return ProviderAnthropic }

func (AnthropicResponseRewriter) MatchesResponse(req *http.Request, resp *http.Response) bool {
	return req != nil && resp != nil && matchAnthropicEndpoint(req)
}

func (rw AnthropicResponseRewriter) Rewrite(body []byte, contentType string, eval ToolUseEvaluator) (RewriteResult, error) {
	if isSSE(contentType) {
		return rw.rewriteSSE(body, eval)
	}
	return rw.rewriteJSON(body, eval)
}

type anthropicJSONResponse struct {
	ID         string                 `json:"id,omitempty"`
	Type       string                 `json:"type,omitempty"`
	Role       string                 `json:"role,omitempty"`
	Model      string                 `json:"model,omitempty"`
	Content    []anthropicJSONContent `json:"content,omitempty"`
	StopReason string                 `json:"stop_reason,omitempty"`
	Usage      json.RawMessage        `json:"usage,omitempty"`
}

type anthropicJSONContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (rw AnthropicResponseRewriter) rewriteJSON(body []byte, eval ToolUseEvaluator) (RewriteResult, error) {
	var resp anthropicJSONResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return RewriteResult{Body: body}, nil
	}
	if resp.Type != "" && resp.Type != "message" {
		return RewriteResult{Body: body}, nil
	}

	var decisions []ToolUseDecisionRecord
	var frags []assistantFragment
	anyBlocked := false
	anyRewritten := false
	index := 0
	for i, block := range resp.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				frags = append(frags, assistantFragment{Text: block.Text})
			}
		case "tool_use":
			tu := ToolUse{
				ID:    block.ID,
				Index: index,
				Name:  block.Name,
				Input: block.Input,
			}
			index++
			verdict := eval(tu)
			decisions = append(decisions, ToolUseDecisionRecord{
				ToolUse:          tu,
				Verdict:          verdict,
				ToolInputPreview: MakeToolInputPreview(block.Input),
			})
			if !verdict.Allowed {
				anyBlocked = true
			}
			finalInput := block.Input
			if verdict.Allowed && len(verdict.RewriteInput) > 0 {
				resp.Content[i].Input = verdict.RewriteInput
				finalInput = verdict.RewriteInput
				anyRewritten = true
			}
			frags = append(frags, assistantFragment{
				IsTool:   true,
				ToolName: block.Name,
				ToolArgs: finalInput,
			})
		}
	}

	turn := assistantTurnFromFragments(frags, decisions)
	if !anyBlocked && anyRewritten {
		// Re-marshal the response with mutated tool_use inputs in place.
		rewritten, err := json.Marshal(resp)
		if err != nil {
			return RewriteResult{}, fmt.Errorf("anthropic: marshal rewritten response: %w", err)
		}
		return RewriteResult{
			Body:          rewritten,
			Decisions:     decisions,
			Rewritten:     true,
			AssistantTurn: turn,
		}, nil
	}
	if !anyBlocked {
		return RewriteResult{Body: body, Decisions: decisions, AssistantTurn: turn}, nil
	}

	out := anthropicJSONResponse{
		ID:    resp.ID,
		Type:  "message",
		Role:  resp.Role,
		Model: resp.Model,
		Content: []anthropicJSONContent{
			{Type: "text", Text: blockedReasonText(decisions)},
		},
		StopReason: "end_turn",
		Usage:      resp.Usage,
	}
	rewritten, err := json.Marshal(out)
	if err != nil {
		return RewriteResult{}, fmt.Errorf("anthropic: marshal rewritten response: %w", err)
	}
	return RewriteResult{
		Body:          rewritten,
		Decisions:     decisions,
		Rewritten:     true,
		AssistantTurn: turn,
	}, nil
}

type sseEvent struct {
	Event string
	Data  string
}

// pendingBlock buffers a single content block (text or tool_use) as SSE
// events stream in. Lifted to package scope so the multi-block SSE
// re-emitter can accept slices of them.
type pendingBlock struct {
	name  string
	id    string
	input bytes.Buffer
	text  bytes.Buffer
	isTU  bool
}

func (rw AnthropicResponseRewriter) rewriteSSE(body []byte, eval ToolUseEvaluator) (RewriteResult, error) {
	events, err := parseSSEEvents(body)
	if err != nil {
		return RewriteResult{Body: body}, nil
	}

	blocks := map[int]*pendingBlock{}
	var orderedAll []*pendingBlock
	var orderedTUs []*pendingBlock
	var msgID, msgModel, msgRole string

	for _, event := range events {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(event.Data), &raw); err != nil {
			continue
		}
		switch event.Event {
		case "message_start":
			var ms struct {
				Message struct {
					ID    string `json:"id"`
					Role  string `json:"role"`
					Model string `json:"model"`
				} `json:"message"`
			}
			_ = json.Unmarshal([]byte(event.Data), &ms)
			msgID = ms.Message.ID
			msgModel = ms.Message.Model
			msgRole = ms.Message.Role
		case "content_block_start":
			var cbs struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type  string          `json:"type"`
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
					Text  string          `json:"text"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(event.Data), &cbs); err != nil {
				continue
			}
			pb := &pendingBlock{
				name: cbs.ContentBlock.Name,
				id:   cbs.ContentBlock.ID,
				isTU: cbs.ContentBlock.Type == "tool_use",
			}
			if pb.isTU && len(cbs.ContentBlock.Input) > 0 && string(cbs.ContentBlock.Input) != "{}" {
				pb.input.Write(cbs.ContentBlock.Input)
			}
			if !pb.isTU && cbs.ContentBlock.Text != "" {
				pb.text.WriteString(cbs.ContentBlock.Text)
			}
			blocks[cbs.Index] = pb
			orderedAll = append(orderedAll, pb)
			if pb.isTU {
				orderedTUs = append(orderedTUs, pb)
			}
		case "content_block_delta":
			var cbd struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					PartialJSON string `json:"partial_json"`
					Text        string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(event.Data), &cbd); err != nil {
				continue
			}
			pb, ok := blocks[cbd.Index]
			if !ok {
				continue
			}
			switch cbd.Delta.Type {
			case "input_json_delta":
				if pb.isTU {
					pb.input.WriteString(cbd.Delta.PartialJSON)
				}
			case "text_delta":
				if !pb.isTU {
					pb.text.WriteString(cbd.Delta.Text)
				}
			}
		}
	}

	var decisions []ToolUseDecisionRecord
	anyBlocked := false
	anyRewritten := false
	rewrittenInput := map[*pendingBlock]json.RawMessage{}
	for i, pb := range orderedTUs {
		var inputRaw json.RawMessage
		if pb.input.Len() > 0 {
			inputRaw = json.RawMessage(pb.input.Bytes())
		}
		tu := ToolUse{
			ID:    pb.id,
			Index: i,
			Name:  pb.name,
			Input: inputRaw,
		}
		verdict := eval(tu)
		decisions = append(decisions, ToolUseDecisionRecord{
			ToolUse:          tu,
			Verdict:          verdict,
			ToolInputPreview: MakeToolInputPreview(inputRaw),
		})
		if !verdict.Allowed {
			anyBlocked = true
		}
		if verdict.Allowed && len(verdict.RewriteInput) > 0 {
			rewrittenInput[pb] = verdict.RewriteInput
			anyRewritten = true
		}
	}

	frags := make([]assistantFragment, 0, len(orderedAll))
	for _, pb := range orderedAll {
		if pb.isTU {
			var inputRaw json.RawMessage
			if rw, ok := rewrittenInput[pb]; ok {
				inputRaw = rw
			} else if pb.input.Len() > 0 {
				inputRaw = json.RawMessage(pb.input.Bytes())
			}
			frags = append(frags, assistantFragment{
				IsTool:   true,
				ToolName: pb.name,
				ToolArgs: inputRaw,
			})
			continue
		}
		if pb.text.Len() > 0 {
			frags = append(frags, assistantFragment{Text: pb.text.String()})
		}
	}
	turn := assistantTurnFromFragments(frags, decisions)
	if !anyBlocked && anyRewritten {
		// Re-emit the entire turn as SSE with the rewritten input bytes
		// substituted for the affected tool_use blocks.
		assembled, err := buildAnthropicMultiBlockSSE(msgID, msgModel, msgRole, orderedAll, rewrittenInput)
		if err != nil {
			return RewriteResult{}, err
		}
		return RewriteResult{
			Body:          assembled,
			Decisions:     decisions,
			Rewritten:     true,
			AssistantTurn: turn,
		}, nil
	}
	if !anyBlocked {
		return RewriteResult{Body: body, Decisions: decisions, AssistantTurn: turn}, nil
	}

	return RewriteResult{
		Body:          synthAnthropicTextSSE(msgID, msgModel, msgRole, blockedReasonText(decisions)),
		Decisions:     decisions,
		Rewritten:     true,
		AssistantTurn: turn,
	}, nil
}

// buildAnthropicMultiBlockSSE re-emits a buffered Anthropic streamed
// response as SSE bytes, substituting the rewritten input bytes for any
// tool_use blocks the inspector decided to redirect through the resolver.
//
// The output is a self-contained SSE message: message_start, then per
// block content_block_start + delta(s) + content_block_stop, then
// message_delta + message_stop. Block indices are 0..N-1 contiguous;
// the upstream's original indices are not preserved (which is fine —
// stop_reason and overall structure are what the harness cares about).
func buildAnthropicMultiBlockSSE(msgID, model, role string, blocks []*pendingBlock, rewrittenInput map[*pendingBlock]json.RawMessage) ([]byte, error) {
	if msgID == "" {
		msgID = "msg_clawvisor_rewrite"
	}
	if model == "" {
		model = "unknown"
	}
	if role == "" {
		role = "assistant"
	}

	var b bytes.Buffer
	emit := func(name string, data any) error {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteString("\ndata: ")
		b.Write(raw)
		b.WriteString("\n\n")
		return nil
	}

	if err := emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          role,
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		return nil, err
	}

	stopReason := "end_turn"
	outIndex := 0
	for _, pb := range blocks {
		if pb.isTU {
			stopReason = "tool_use"
			if err := emit("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": outIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    pb.id,
					"name":  pb.name,
					"input": map[string]any{},
				},
			}); err != nil {
				return nil, err
			}
			input := pb.input.Bytes()
			if rw, ok := rewrittenInput[pb]; ok {
				input = rw
			}
			if err := emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": outIndex,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": string(input),
				},
			}); err != nil {
				return nil, err
			}
			if err := emit("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": outIndex,
			}); err != nil {
				return nil, err
			}
			outIndex++
			continue
		}
		if pb.text.Len() == 0 {
			continue
		}
		// Text block.
		if err := emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": outIndex,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		}); err != nil {
			return nil, err
		}
		if pb.text.Len() > 0 {
			if err := emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": outIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": pb.text.String(),
				},
			}); err != nil {
				return nil, err
			}
		}
		if err := emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": outIndex,
		}); err != nil {
			return nil, err
		}
		outIndex++
	}

	if err := emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": 0},
	}); err != nil {
		return nil, err
	}
	if err := emit("message_stop", map[string]any{
		"type": "message_stop",
	}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func SynthAnthropicTextSSE(msgID, model, role, text string) []byte {
	return synthAnthropicTextSSE(msgID, model, role, text)
}

func SynthAnthropicTextJSON(msgID, model, role, text string) []byte {
	if msgID == "" {
		msgID = "msg_clawvisor_block"
	}
	if model == "" {
		model = "unknown"
	}
	if role == "" {
		role = "assistant"
	}
	out := anthropicJSONResponse{
		ID:    msgID,
		Type:  "message",
		Role:  role,
		Model: model,
		Content: []anthropicJSONContent{
			{Type: "text", Text: text},
		},
		StopReason: "end_turn",
	}
	body, _ := json.Marshal(out)
	return body
}

func SynthAnthropicToolUseSSE(msgID, model, role, toolUseID, toolName string, toolInput map[string]any) []byte {
	if msgID == "" {
		msgID = "msg_clawvisor_approve"
	}
	if model == "" {
		model = "unknown"
	}
	if role == "" {
		role = "assistant"
	}
	if toolInput == nil {
		toolInput = map[string]any{}
	}
	inputJSON, err := json.Marshal(toolInput)
	if err != nil {
		inputJSON = []byte("{}")
	}

	var b bytes.Buffer
	emit := func(name string, data any) {
		raw, _ := json.Marshal(data)
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteString("\ndata: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          role,
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    toolUseID,
			"name":  toolName,
			"input": map[string]any{},
		},
	})
	emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": string(inputJSON),
		},
	})
	emit("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})
	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   "tool_use",
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": 0},
	})
	emit("message_stop", map[string]any{
		"type": "message_stop",
	})
	return b.Bytes()
}

// SynthAnthropicToolUsesJSON builds an Anthropic JSON message-shaped
// response carrying N tool_use content blocks. Used by the coalesced-
// approval release path to surface every approved call in one assistant
// turn. Byte-identical to SynthAnthropicToolUseJSON when len(calls)==1.
func SynthAnthropicToolUsesJSON(msgID, model, role string, calls []SyntheticToolCall) []byte {
	if msgID == "" {
		msgID = "msg_clawvisor_approve"
	}
	if model == "" {
		model = "unknown"
	}
	if role == "" {
		role = "assistant"
	}
	content := make([]anthropicJSONContent, 0, len(calls))
	for _, call := range calls {
		input := call.Input
		if input == nil {
			input = map[string]any{}
		}
		inputJSON, err := json.Marshal(input)
		if err != nil {
			inputJSON = []byte("{}")
		}
		content = append(content, anthropicJSONContent{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Name,
			Input: inputJSON,
		})
	}
	out := anthropicJSONResponse{
		ID:         msgID,
		Type:       "message",
		Role:       role,
		Model:      model,
		Content:    content,
		StopReason: "tool_use",
	}
	body, _ := json.Marshal(out)
	return body
}

// SynthAnthropicToolUsesSSE is the SSE counterpart to
// SynthAnthropicToolUsesJSON: one self-contained Anthropic streamed
// message carrying N tool_use blocks at sequential indices.
func SynthAnthropicToolUsesSSE(msgID, model, role string, calls []SyntheticToolCall) []byte {
	if msgID == "" {
		msgID = "msg_clawvisor_approve"
	}
	if model == "" {
		model = "unknown"
	}
	if role == "" {
		role = "assistant"
	}
	var b bytes.Buffer
	emit := func(name string, data any) {
		raw, _ := json.Marshal(data)
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteString("\ndata: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          role,
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	for i, call := range calls {
		input := call.Input
		if input == nil {
			input = map[string]any{}
		}
		inputJSON, err := json.Marshal(input)
		if err != nil {
			inputJSON = []byte("{}")
		}
		emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": i,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    call.ID,
				"name":  call.Name,
				"input": map[string]any{},
			},
		})
		emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": i,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": string(inputJSON),
			},
		})
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": i,
		})
	}
	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   "tool_use",
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": 0},
	})
	emit("message_stop", map[string]any{
		"type": "message_stop",
	})
	return b.Bytes()
}

func SynthAnthropicToolUseJSON(msgID, model, role, toolUseID, toolName string, toolInput map[string]any) []byte {
	if msgID == "" {
		msgID = "msg_clawvisor_approve"
	}
	if model == "" {
		model = "unknown"
	}
	if role == "" {
		role = "assistant"
	}
	inputJSON, _ := json.Marshal(toolInput)
	out := anthropicJSONResponse{
		ID:    msgID,
		Type:  "message",
		Role:  role,
		Model: model,
		Content: []anthropicJSONContent{
			{
				Type:  "tool_use",
				ID:    toolUseID,
				Name:  toolName,
				Input: inputJSON,
			},
		},
		StopReason: "tool_use",
	}
	body, _ := json.Marshal(out)
	return body
}

// ExtractAnthropicAssistantContent reconstructs the assistant message's
// content[] array from an upstream /v1/messages response. Handles both
// JSON (single-shot) and SSE (streamed) wire formats. Returned as a
// json.RawMessage that round-trips through json.Marshal so callers can
// splice it back into a continuation request's messages array.
//
// Returns an error if the body is malformed or yields no content blocks.
func ExtractAnthropicAssistantContent(contentType string, body []byte) (json.RawMessage, error) {
	if isSSE(contentType) {
		return extractAnthropicAssistantContentSSE(body)
	}
	return extractAnthropicAssistantContentJSON(body)
}

func extractAnthropicAssistantContentJSON(body []byte) (json.RawMessage, error) {
	var resp struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("conversation: parse anthropic JSON response: %w", err)
	}
	if len(resp.Content) == 0 {
		return nil, fmt.Errorf("conversation: anthropic JSON response has no content")
	}
	return resp.Content, nil
}

// extractAnthropicAssistantContentSSE walks the event stream and
// rebuilds the structured content[] array. The bookkeeping mirrors
// rewriteSSE — pendingBlock-style accumulators per content index — but
// produces a structured shape (text + tool_use objects with parsed
// input) suitable for resending to the upstream as a prior assistant
// turn.
func extractAnthropicAssistantContentSSE(body []byte) (json.RawMessage, error) {
	events, err := parseSSEEvents(body)
	if err != nil {
		return nil, fmt.Errorf("conversation: parse anthropic SSE: %w", err)
	}
	type pending struct {
		isTU     bool
		toolID   string
		toolName string
		input    strings.Builder
		text     strings.Builder
	}
	blocks := map[int]*pending{}
	var order []int
	for _, ev := range events {
		switch ev.Event {
		case "content_block_start":
			var cbs struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type  string          `json:"type"`
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
					Text  string          `json:"text"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &cbs); err != nil {
				continue
			}
			p := &pending{
				isTU:     cbs.ContentBlock.Type == "tool_use",
				toolID:   cbs.ContentBlock.ID,
				toolName: cbs.ContentBlock.Name,
			}
			if p.isTU && len(cbs.ContentBlock.Input) > 0 && string(cbs.ContentBlock.Input) != "{}" {
				p.input.Write(cbs.ContentBlock.Input)
			}
			if !p.isTU && cbs.ContentBlock.Text != "" {
				p.text.WriteString(cbs.ContentBlock.Text)
			}
			blocks[cbs.Index] = p
			order = append(order, cbs.Index)
		case "content_block_delta":
			var cbd struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					PartialJSON string `json:"partial_json"`
					Text        string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &cbd); err != nil {
				continue
			}
			p, ok := blocks[cbd.Index]
			if !ok {
				continue
			}
			switch cbd.Delta.Type {
			case "input_json_delta":
				if p.isTU {
					p.input.WriteString(cbd.Delta.PartialJSON)
				}
			case "text_delta":
				if !p.isTU {
					p.text.WriteString(cbd.Delta.Text)
				}
			}
		}
	}
	out := make([]any, 0, len(order))
	for _, idx := range order {
		p, ok := blocks[idx]
		if !ok {
			continue
		}
		if p.isTU {
			// Empty input is a valid tool_use shape; the upstream API
			// rejects a tool_use with no input at all so default to {}.
			var input any = map[string]any{}
			if p.input.Len() > 0 {
				if err := json.Unmarshal([]byte(p.input.String()), &input); err != nil {
					input = map[string]any{}
				}
			}
			out = append(out, map[string]any{
				"type":  "tool_use",
				"id":    p.toolID,
				"name":  p.toolName,
				"input": input,
			})
			continue
		}
		if p.text.Len() == 0 {
			continue
		}
		out = append(out, map[string]any{
			"type": "text",
			"text": p.text.String(),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("conversation: anthropic SSE yielded no content blocks")
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("conversation: marshal anthropic SSE content: %w", err)
	}
	return encoded, nil
}

func AnthropicRequestWantsStream(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream
}

func parseSSEEvents(body []byte) ([]sseEvent, error) {
	var out []sseEvent
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)

	var (
		curEvent string
		dataLns  []string
		flush    = func() {
			if len(dataLns) == 0 {
				curEvent = ""
				return
			}
			data := strings.Join(dataLns, "\n")
			if data != "" && data != "[DONE]" {
				name := curEvent
				if name == "" {
					name = "message"
				}
				out = append(out, sseEvent{Event: name, Data: data})
			}
			curEvent = ""
			dataLns = dataLns[:0]
		}
	)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLns = append(dataLns, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	return out, scanner.Err()
}

func synthAnthropicTextSSE(msgID, model, role, text string) []byte {
	if msgID == "" {
		msgID = "msg_clawvisor_block"
	}
	if model == "" {
		model = "unknown"
	}
	if role == "" {
		role = "assistant"
	}

	var b bytes.Buffer
	emit := func(name string, data any) {
		raw, _ := json.Marshal(data)
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteString("\ndata: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          role,
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
	emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	})
	emit("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})
	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": 0},
	})
	emit("message_stop", map[string]any{
		"type": "message_stop",
	})
	return b.Bytes()
}
