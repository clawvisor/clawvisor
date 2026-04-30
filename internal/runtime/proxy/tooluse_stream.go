package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func (s *Server) tryStreamToolUseBlock(req *http.Request, resp *http.Response, st *RequestState, hooks ToolUseHooks, evaluator conversation.ToolUseEvaluator, decisionState map[string]toolDecisionState) bool {
	if req == nil || resp == nil || resp.Body == nil || !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return false
	}
	switch {
	case conversation.MatchProviderAnthropic(req):
		resp.Body = newToolUseStreamBody(resp.Body, newAnthropicToolUseStreamProcessor(s, req.Context(), st, hooks, evaluator, decisionState))
		return true
	case conversation.MatchProviderOpenAI(req):
		if conversation.IsOpenAIChatCompletionsEndpoint(req) {
			resp.Body = newToolUseStreamBody(resp.Body, newOpenAIChatToolUseStreamProcessor(s, req.Context(), st, hooks, evaluator, decisionState))
			return true
		}
		resp.Body = newToolUseStreamBody(resp.Body, newOpenAIResponsesToolUseStreamProcessor(s, req.Context(), st, hooks, evaluator, decisionState))
		return true
	default:
		return false
	}
}

type toolUseStreamProcessor interface {
	ProcessBlock(raw []byte) (emit []byte, stop bool, err error)
	Finish() ([]byte, error)
}

type toolUseStreamBody struct {
	upstream  io.ReadCloser
	reader    *bufio.Reader
	processor toolUseStreamProcessor
	out       bytes.Buffer
	finished  bool
	readErr   error
}

func newToolUseStreamBody(upstream io.ReadCloser, processor toolUseStreamProcessor) io.ReadCloser {
	return &toolUseStreamBody{
		upstream:  upstream,
		reader:    bufio.NewReader(upstream),
		processor: processor,
	}
}

func (b *toolUseStreamBody) Read(p []byte) (int, error) {
	for b.out.Len() == 0 && !b.finished && b.readErr == nil {
		if err := b.fill(); err != nil {
			b.readErr = err
			break
		}
	}
	if b.out.Len() > 0 {
		return b.out.Read(p)
	}
	if b.readErr != nil {
		err := b.readErr
		b.readErr = nil
		return 0, err
	}
	if b.finished {
		return 0, io.EOF
	}
	return 0, nil
}

func (b *toolUseStreamBody) Close() error {
	return b.upstream.Close()
}

func (b *toolUseStreamBody) fill() error {
	raw, err := readNextSSEBlock(b.reader)
	if err == io.EOF {
		b.finished = true
		tail, finishErr := b.processor.Finish()
		if finishErr != nil {
			return finishErr
		}
		b.out.Write(tail)
		return nil
	}
	if err != nil {
		return err
	}
	emit, stop, err := b.processor.ProcessBlock(raw)
	if err != nil {
		return err
	}
	b.out.Write(emit)
	if stop {
		b.finished = true
		_ = b.upstream.Close()
	}
	return nil
}

func readNextSSEBlock(r *bufio.Reader) ([]byte, error) {
	var raw bytes.Buffer
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			raw.Write(line)
			if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
				return raw.Bytes(), nil
			}
		}
		if err != nil {
			if err == io.EOF && raw.Len() > 0 {
				return raw.Bytes(), nil
			}
			return nil, err
		}
	}
}

func parseSSEBlock(raw []byte) (event string, data string, ok bool) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var dataLines []string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(dataLines) == 0 {
		return "", "", false
	}
	if event == "" {
		event = "message"
	}
	data = strings.Join(dataLines, "\n")
	if data == "" || data == "[DONE]" {
		return event, data, false
	}
	return event, data, true
}

type anthropicToolUseStreamProcessor struct {
	server        *Server
	ctx           context.Context
	reqState      *RequestState
	hooks         ToolUseHooks
	evaluator     conversation.ToolUseEvaluator
	decisionState map[string]toolDecisionState
	pending       map[int]*anthropicPendingToolUse
	nextToolIndex int
}

type anthropicPendingToolUse struct {
	index int
	id    string
	name  string
	raw   bytes.Buffer
	input bytes.Buffer
}

func newAnthropicToolUseStreamProcessor(server *Server, ctx context.Context, st *RequestState, hooks ToolUseHooks, evaluator conversation.ToolUseEvaluator, decisionState map[string]toolDecisionState) *anthropicToolUseStreamProcessor {
	return &anthropicToolUseStreamProcessor{
		server:        server,
		ctx:           ctx,
		reqState:      st,
		hooks:         hooks,
		evaluator:     evaluator,
		decisionState: decisionState,
		pending:       make(map[int]*anthropicPendingToolUse),
	}
}

func (p *anthropicToolUseStreamProcessor) ProcessBlock(raw []byte) ([]byte, bool, error) {
	event, data, ok := parseSSEBlock(raw)
	if !ok {
		return raw, false, nil
	}
	switch event {
	case "content_block_start":
		var msg struct {
			Index int `json:"index"`
			Block struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			return raw, false, nil
		}
		if msg.Block.Type != "tool_use" {
			return raw, false, nil
		}
		pb := &anthropicPendingToolUse{index: msg.Index, id: msg.Block.ID, name: msg.Block.Name}
		pb.raw.Write(raw)
		if len(msg.Block.Input) > 0 && string(msg.Block.Input) != "{}" {
			pb.input.Write(msg.Block.Input)
		}
		p.pending[msg.Index] = pb
		return nil, false, nil
	case "content_block_delta":
		var msg struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			return raw, false, nil
		}
		pb := p.pending[msg.Index]
		if pb == nil {
			return raw, false, nil
		}
		pb.raw.Write(raw)
		if msg.Delta.Type == "input_json_delta" {
			pb.input.WriteString(msg.Delta.PartialJSON)
		}
		return nil, false, nil
	case "content_block_stop":
		var msg struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			return raw, false, nil
		}
		pb := p.pending[msg.Index]
		if pb == nil {
			return raw, false, nil
		}
		delete(p.pending, msg.Index)
		pb.raw.Write(raw)
		inputRaw := json.RawMessage(pb.input.Bytes())
		tu := conversation.ToolUse{ID: pb.id, Index: p.nextToolIndex, Name: pb.name, Input: inputRaw}
		p.nextToolIndex++
		decision := conversation.ToolUseDecisionRecord{
			ToolUse:          tu,
			Verdict:          p.evaluator(tu),
			ToolInputPreview: conversation.MakeToolInputPreview(inputRaw),
		}
		p.server.applyToolUseDecision(p.ctx, p.hooks, p.reqState, decision, p.decisionState[toolDecisionKey(decision.ToolUse)])
		if decision.Verdict.Allowed {
			return pb.raw.Bytes(), false, nil
		}
		return synthAnthropicTextSSETail(pb.index, blockedTextForStreaming(decision)), true, nil
	default:
		return raw, false, nil
	}
}

func (p *anthropicToolUseStreamProcessor) Finish() ([]byte, error) { return nil, nil }

type openAIResponsesToolUseStreamProcessor struct {
	server        *Server
	ctx           context.Context
	reqState      *RequestState
	hooks         ToolUseHooks
	evaluator     conversation.ToolUseEvaluator
	decisionState map[string]toolDecisionState
	pending       map[string]*openAIResponsesPendingToolUse
}

type openAIResponsesPendingToolUse struct {
	itemID      string
	callID      string
	name        string
	outputIndex int
	raw         bytes.Buffer
	arguments   strings.Builder
}

func newOpenAIResponsesToolUseStreamProcessor(server *Server, ctx context.Context, st *RequestState, hooks ToolUseHooks, evaluator conversation.ToolUseEvaluator, decisionState map[string]toolDecisionState) *openAIResponsesToolUseStreamProcessor {
	return &openAIResponsesToolUseStreamProcessor{
		server:        server,
		ctx:           ctx,
		reqState:      st,
		hooks:         hooks,
		evaluator:     evaluator,
		decisionState: decisionState,
		pending:       make(map[string]*openAIResponsesPendingToolUse),
	}
}

func (p *openAIResponsesToolUseStreamProcessor) ProcessBlock(raw []byte) ([]byte, bool, error) {
	event, data, ok := parseSSEBlock(raw)
	if !ok {
		return raw, false, nil
	}
	switch event {
	case "response.output_item.added":
		var msg struct {
			OutputIndex int `json:"output_index"`
			Item        struct {
				ID        string `json:"id"`
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments any    `json:"arguments"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			return raw, false, nil
		}
		if msg.Item.Type != "function_call" {
			return raw, false, nil
		}
		pc := &openAIResponsesPendingToolUse{
			itemID:      msg.Item.ID,
			callID:      firstNonEmpty(msg.Item.CallID, msg.Item.ID),
			name:        msg.Item.Name,
			outputIndex: msg.OutputIndex,
		}
		pc.raw.Write(raw)
		if args := stringifyOpenAIStreamArguments(msg.Item.Arguments); args != "" {
			pc.arguments.WriteString(args)
		}
		p.pending[msg.Item.ID] = pc
		return nil, false, nil
	case "response.function_call_arguments.delta":
		var msg struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			return raw, false, nil
		}
		pc := p.pending[msg.ItemID]
		if pc == nil {
			return raw, false, nil
		}
		pc.raw.Write(raw)
		pc.arguments.WriteString(msg.Delta)
		return nil, false, nil
	case "response.function_call_arguments.done":
		var msg struct {
			ItemID    string `json:"item_id"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			return raw, false, nil
		}
		pc := p.pending[msg.ItemID]
		if pc == nil {
			return raw, false, nil
		}
		pc.raw.Write(raw)
		if msg.Arguments != "" {
			pc.arguments.Reset()
			pc.arguments.WriteString(msg.Arguments)
		}
		return nil, false, nil
	case "response.output_item.done":
		var msg struct {
			OutputIndex int `json:"output_index"`
			Item        struct {
				ID        string `json:"id"`
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments any    `json:"arguments"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			return raw, false, nil
		}
		if msg.Item.Type != "function_call" {
			return raw, false, nil
		}
		pc := p.pending[msg.Item.ID]
		if pc == nil {
			return raw, false, nil
		}
		delete(p.pending, msg.Item.ID)
		pc.raw.Write(raw)
		if args := stringifyOpenAIStreamArguments(msg.Item.Arguments); args != "" {
			pc.arguments.Reset()
			pc.arguments.WriteString(args)
		}
		tu := conversation.ToolUse{
			ID:    pc.callID,
			Index: msg.OutputIndex,
			Name:  pc.name,
			Input: rawIfJSONOpenAIStream(pc.arguments.String()),
		}
		decision := conversation.ToolUseDecisionRecord{
			ToolUse:          tu,
			Verdict:          p.evaluator(tu),
			ToolInputPreview: conversation.MakeToolInputPreview(tu.Input),
		}
		p.server.applyToolUseDecision(p.ctx, p.hooks, p.reqState, decision, p.decisionState[toolDecisionKey(decision.ToolUse)])
		if decision.Verdict.Allowed {
			return pc.raw.Bytes(), false, nil
		}
		return synthOpenAIResponsesTextSSETail(pc.outputIndex, blockedTextForStreaming(decision)), true, nil
	default:
		return raw, false, nil
	}
}

func (p *openAIResponsesToolUseStreamProcessor) Finish() ([]byte, error) { return nil, nil }

type openAIChatToolUseStreamProcessor struct {
	server        *Server
	ctx           context.Context
	reqState      *RequestState
	hooks         ToolUseHooks
	evaluator     conversation.ToolUseEvaluator
	decisionState map[string]toolDecisionState
	pendingRaw    bytes.Buffer
	pending       map[int]*openAIChatPendingToolUse
}

type openAIChatPendingToolUse struct {
	id   string
	name string
	args strings.Builder
}

func newOpenAIChatToolUseStreamProcessor(server *Server, ctx context.Context, st *RequestState, hooks ToolUseHooks, evaluator conversation.ToolUseEvaluator, decisionState map[string]toolDecisionState) *openAIChatToolUseStreamProcessor {
	return &openAIChatToolUseStreamProcessor{
		server:        server,
		ctx:           ctx,
		reqState:      st,
		hooks:         hooks,
		evaluator:     evaluator,
		decisionState: decisionState,
		pending:       make(map[int]*openAIChatPendingToolUse),
	}
}

func (p *openAIChatToolUseStreamProcessor) ProcessBlock(raw []byte) ([]byte, bool, error) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var payload string
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			break
		}
	}
	if payload == "" || payload == "[DONE]" {
		if payload == "[DONE]" && p.pendingRaw.Len() > 0 {
			return p.finishPendingToolCalls()
		}
		return raw, false, nil
	}
	var msg struct {
		Choices []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
			Delta        struct {
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		return raw, false, nil
	}
	containsToolCalls := false
	finishesToolCalls := false
	for _, choice := range msg.Choices {
		if len(choice.Delta.ToolCalls) > 0 {
			containsToolCalls = true
		}
		if choice.FinishReason == "tool_calls" {
			finishesToolCalls = true
		}
		for _, tc := range choice.Delta.ToolCalls {
			pc := p.pending[tc.Index]
			if pc == nil {
				pc = &openAIChatPendingToolUse{}
				p.pending[tc.Index] = pc
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
	if !containsToolCalls && !finishesToolCalls {
		return raw, false, nil
	}
	p.pendingRaw.Write(raw)
	if !finishesToolCalls {
		return nil, false, nil
	}
	return p.finishPendingToolCalls()
}

func (p *openAIChatToolUseStreamProcessor) Finish() ([]byte, error) {
	if p.pendingRaw.Len() == 0 {
		return nil, nil
	}
	emit, _, err := p.finishPendingToolCalls()
	return emit, err
}

func (p *openAIChatToolUseStreamProcessor) finishPendingToolCalls() ([]byte, bool, error) {
	indexes := make([]int, 0, len(p.pending))
	for index := range p.pending {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	var decisions []conversation.ToolUseDecisionRecord
	for _, index := range indexes {
		pc := p.pending[index]
		tu := conversation.ToolUse{
			ID:    pc.id,
			Index: index,
			Name:  pc.name,
			Input: rawIfJSONOpenAIStream(pc.args.String()),
		}
		decision := conversation.ToolUseDecisionRecord{
			ToolUse:          tu,
			Verdict:          p.evaluator(tu),
			ToolInputPreview: conversation.MakeToolInputPreview(tu.Input),
		}
		decisions = append(decisions, decision)
		p.server.applyToolUseDecision(p.ctx, p.hooks, p.reqState, decision, p.decisionState[toolDecisionKey(decision.ToolUse)])
	}
	defer func() {
		p.pendingRaw.Reset()
		p.pending = make(map[int]*openAIChatPendingToolUse)
	}()
	for _, decision := range decisions {
		if !decision.Verdict.Allowed {
			return synthOpenAIChatTextSSETail(blockedTextForStreaming(decision)), true, nil
		}
	}
	return append([]byte(nil), p.pendingRaw.Bytes()...), false, nil
}

func blockedTextForStreaming(decision conversation.ToolUseDecisionRecord) string {
	if decision.Verdict.SubstituteWith != "" {
		return decision.Verdict.SubstituteWith
	}
	return "Tool use was blocked by the Clawvisor proxy:\n- " + decision.ToolUse.Name + ": " + firstNonEmpty(decision.Verdict.Reason, "blocked by policy")
}

func synthAnthropicTextSSETail(index int, text string) []byte {
	var b bytes.Buffer
	emit := func(name string, data any) {
		raw, _ := json.Marshal(data)
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteString("\ndata: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
	emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	})
	emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": 0},
	})
	emit("message_stop", map[string]any{"type": "message_stop"})
	return b.Bytes()
}

func synthOpenAIResponsesTextSSETail(outputIndex int, text string) []byte {
	var b strings.Builder
	b.WriteString(sseEventBlock("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": outputIndex,
		"item":         map[string]any{"id": "msg_clawvisor_block", "type": "message", "role": "assistant", "status": "in_progress"},
	}))
	b.WriteString(sseEventBlock("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       "msg_clawvisor_block",
		"output_index":  outputIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": ""},
	}))
	b.WriteString(sseEventBlock("response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       "msg_clawvisor_block",
		"output_index":  outputIndex,
		"content_index": 0,
		"delta":         text,
	}))
	b.WriteString(sseEventBlock("response.output_text.done", map[string]any{
		"type":          "response.output_text.done",
		"item_id":       "msg_clawvisor_block",
		"output_index":  outputIndex,
		"content_index": 0,
		"text":          text,
	}))
	b.WriteString(sseEventBlock("response.content_part.done", map[string]any{
		"type":          "response.content_part.done",
		"item_id":       "msg_clawvisor_block",
		"output_index":  outputIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": text},
	}))
	b.WriteString(sseEventBlock("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": outputIndex,
		"item":         map[string]any{"id": "msg_clawvisor_block", "type": "message", "role": "assistant", "status": "completed"},
	}))
	b.WriteString(sseEventBlock("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_clawvisor_block", "status": "completed"}}))
	return []byte(b.String())
}

func synthOpenAIChatTextSSETail(text string) []byte {
	var b strings.Builder
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

func sseEventBlock(event string, data any) string {
	raw, _ := json.Marshal(data)
	return "event: " + event + "\ndata: " + string(raw) + "\n\n"
}

func chatCompletionSSEBlock(data any) string {
	raw, _ := json.Marshal(data)
	return "data: " + string(raw) + "\n\n"
}

func stringifyOpenAIStreamArguments(v any) string {
	switch typed := v.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		if v == nil {
			return ""
		}
		raw, _ := json.Marshal(v)
		s := strings.TrimSpace(string(raw))
		if strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
			var unquoted string
			if err := json.Unmarshal(raw, &unquoted); err == nil {
				return strings.TrimSpace(unquoted)
			}
		}
		return s
	}
}

func rawIfJSONOpenAIStream(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" || !json.Valid([]byte(args)) {
		return nil
	}
	return json.RawMessage(args)
}
