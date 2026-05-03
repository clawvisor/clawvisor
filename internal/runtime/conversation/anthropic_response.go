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
	index := 0
	for _, block := range resp.Content {
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
			frags = append(frags, assistantFragment{
				IsTool:   true,
				ToolName: block.Name,
				ToolArgs: block.Input,
			})
		}
	}

	turn := assistantTurnFromFragments(frags, decisions)
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

func (rw AnthropicResponseRewriter) rewriteSSE(body []byte, eval ToolUseEvaluator) (RewriteResult, error) {
	events, err := parseSSEEvents(body)
	if err != nil {
		return RewriteResult{Body: body}, nil
	}

	type pendingBlock struct {
		name  string
		id    string
		input bytes.Buffer
		text  bytes.Buffer
		isTU  bool
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
	}

	frags := make([]assistantFragment, 0, len(orderedAll))
	for _, pb := range orderedAll {
		if pb.isTU {
			var inputRaw json.RawMessage
			if pb.input.Len() > 0 {
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
