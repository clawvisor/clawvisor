package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/elazarl/goproxy"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

const observeNoticeEventType = "runtime.observe.notice"

var observeNoticeInterval = 24 * time.Hour
var observeNoticePrefixRE = regexp.MustCompile(`^(?:\(\[Clawvisor system message\]: Clawvisor is currently running in observe mode\. Actions are being analyzed and logged, but not blocked\.(?: Change this in Clawvisor: [^)]+)?\)|\(Clawvisor is in observe mode\. Actions are being analyzed and logged, but not blocked\.\)|Clawvisor is in observe mode\. Actions are being analyzed and logged, but not blocked\.)(?:(?:\s*\n\s*\n|\s+)|$)`)

type responseNotice struct {
	Kind string
	Text string
}

func (s *Server) InstallObserveNoticeRequestScrubber() {
	s.goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if req == nil || req.Body == nil {
			return req, nil
		}
		switch {
		case conversation.MatchProviderAnthropic(req), conversation.MatchProviderOpenAI(req):
		default:
			return req, nil
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return req, nil
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		rewritten, changed := scrubHistoricalResponseNoticesFromRequest(req, body)
		if !changed {
			return req, nil
		}
		req.Body = io.NopCloser(bytes.NewReader(rewritten))
		req.ContentLength = int64(len(rewritten))
		return req, nil
	})
}

func observeModeInjectedUserNotice(agentID, dashboardBaseURL string) string {
	link := observeModeDashboardLink(agentID, dashboardBaseURL)
	if link == "" {
		return "([Clawvisor system message]: Clawvisor is currently running in observe mode. Actions are being analyzed and logged, but not blocked.)"
	}
	return "([Clawvisor system message]: Clawvisor is currently running in observe mode. Actions are being analyzed and logged, but not blocked. Change this in Clawvisor: " + link + ")"
}

func scrubHistoricalResponseNoticesFromRequest(req *http.Request, body []byte) ([]byte, bool) {
	if req == nil || len(body) == 0 {
		return body, false
	}
	switch {
	case conversation.MatchProviderAnthropic(req):
		return scrubAnthropicHistoricalResponseNotices(body)
	case conversation.MatchProviderOpenAI(req):
		if conversation.IsOpenAIChatCompletionsEndpoint(req) {
			return scrubOpenAIChatHistoricalResponseNotices(body)
		}
		return scrubOpenAIResponsesHistoricalResponseNotices(body)
	default:
		return body, false
	}
}

func scrubAnthropicHistoricalResponseNotices(body []byte) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return body, false
	}
	changed := false
	for i, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok || strings.TrimSpace(anyString(msg["role"])) != "assistant" {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			if text, ok := msg["content"].(string); ok {
				if scrubbed, changedText := scrubHistoricalResponseNoticeText(text); changedText {
					changed = true
					if strings.TrimSpace(scrubbed) == "" {
						msg["content"] = []any{}
					} else {
						msg["content"] = scrubbed
					}
				}
				messages[i] = msg
			}
			continue
		}
		rewritten := make([]any, 0, len(content))
		for _, blockItem := range content {
			block, ok := blockItem.(map[string]any)
			if !ok {
				rewritten = append(rewritten, blockItem)
				continue
			}
			if anyString(block["type"]) != "text" {
				rewritten = append(rewritten, block)
				continue
			}
			scrubbed, changedText := scrubHistoricalResponseNoticeText(anyString(block["text"]))
			if !changedText {
				rewritten = append(rewritten, block)
				continue
			}
			changed = true
			if strings.TrimSpace(scrubbed) == "" {
				continue
			}
			block["text"] = scrubbed
			rewritten = append(rewritten, block)
		}
		msg["content"] = rewritten
		messages[i] = msg
	}
	if !changed {
		return body, false
	}
	payload["messages"] = messages
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

func scrubOpenAIChatHistoricalResponseNotices(body []byte) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return body, false
	}
	changed := false
	for i, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok || strings.TrimSpace(anyString(msg["role"])) != "assistant" {
			continue
		}
		rewrittenContent, contentChanged := scrubOpenAIMessageContent(msg["content"])
		if !contentChanged {
			continue
		}
		changed = true
		msg["content"] = rewrittenContent
		messages[i] = msg
	}
	if !changed {
		return body, false
	}
	payload["messages"] = messages
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

func scrubOpenAIResponsesHistoricalResponseNotices(body []byte) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) == 0 {
		return body, false
	}
	changed := false
	for i, item := range input {
		entry, ok := item.(map[string]any)
		if !ok || strings.TrimSpace(anyString(entry["type"])) != "message" || strings.TrimSpace(anyString(entry["role"])) != "assistant" {
			continue
		}
		rewrittenContent, contentChanged := scrubOpenAIMessageContent(entry["content"])
		if !contentChanged {
			continue
		}
		changed = true
		entry["content"] = rewrittenContent
		input[i] = entry
	}
	if !changed {
		return body, false
	}
	payload["input"] = input
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

func scrubOpenAIMessageContent(content any) (any, bool) {
	switch value := content.(type) {
	case string:
		scrubbed, changed := scrubHistoricalResponseNoticeText(value)
		return scrubbed, changed
	case []any:
		rewritten := make([]any, 0, len(value))
		changed := false
		for _, blockItem := range value {
			block, ok := blockItem.(map[string]any)
			if !ok {
				rewritten = append(rewritten, blockItem)
				continue
			}
			blockType := anyString(block["type"])
			if blockType != "text" && blockType != "input_text" && blockType != "output_text" {
				rewritten = append(rewritten, block)
				continue
			}
			scrubbed, changedText := scrubHistoricalResponseNoticeText(anyString(block["text"]))
			if !changedText {
				rewritten = append(rewritten, block)
				continue
			}
			changed = true
			if strings.TrimSpace(scrubbed) == "" {
				continue
			}
			block["text"] = scrubbed
			rewritten = append(rewritten, block)
		}
		return rewritten, changed
	default:
		return content, false
	}
}

func scrubHistoricalResponseNoticeText(text string) (string, bool) {
	original := text
	trimmedLeading := strings.TrimLeft(text, " \t\r\n")
	changedAny := false
	for {
		loc := observeNoticePrefixRE.FindStringIndex(trimmedLeading)
		if loc == nil || loc[0] != 0 {
			break
		}
		changedAny = true
		trimmedLeading = strings.TrimLeft(trimmedLeading[loc[1]:], " \t\r\n")
	}
	if !changedAny {
		return original, false
	}
	return trimmedLeading, true
}

func anyString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func observeModeDashboardLink(agentID, dashboardBaseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(dashboardBaseURL), "/")
	path := "/dashboard/agents"
	if strings.TrimSpace(agentID) != "" {
		path += "/" + strings.TrimSpace(agentID)
	}
	if base == "" {
		return path
	}
	return base + path
}

func (s *Server) pendingResponseNotices(ctx context.Context, st store.Store, session *store.RuntimeSession) []responseNotice {
	if s == nil || st == nil || session == nil || !session.ObservationMode {
		return nil
	}
	if !s.shouldEmitObserveNotice(ctx, st, session) {
		return nil
	}
	return []responseNotice{{
		Kind: "observe_mode",
		Text: observeModeInjectedUserNotice(session.AgentID, s.cfg.DashboardBaseURL),
	}}
}

func (s *Server) shouldEmitObserveNotice(ctx context.Context, st store.Store, session *store.RuntimeSession) bool {
	if s == nil || st == nil || session == nil || session.ID == "" {
		return false
	}
	now := time.Now().UTC()
	if last, ok := s.observeNoticeBySession.Load(session.ID); ok {
		if ts, ok := last.(time.Time); ok && now.Sub(ts) < observeNoticeInterval {
			return false
		}
	}
	events, err := st.ListRuntimeEvents(ctx, session.UserID, store.RuntimeEventFilter{
		SessionID: session.ID,
		EventType: observeNoticeEventType,
		Limit:     1,
	})
	if err == nil {
		for _, event := range events {
			if event == nil || event.EventType != observeNoticeEventType {
				continue
			}
			if now.Sub(event.Timestamp) < observeNoticeInterval {
				s.observeNoticeBySession.Store(session.ID, event.Timestamp)
				return false
			}
			break
		}
	}
	return true
}

func (s *Server) markObserveNoticeEmitted(sessionID string) {
	if s == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	s.observeNoticeBySession.Store(sessionID, time.Now().UTC())
}

func (s *Server) markResponseNoticesInjected(ctx context.Context, st store.Store, session *store.RuntimeSession, reqState *RequestState, provider conversation.Provider, notices []responseNotice) {
	if s == nil || st == nil || session == nil || len(notices) == 0 {
		return
	}
	for _, notice := range notices {
		switch notice.Kind {
		case "observe_mode":
			s.markObserveNoticeEmitted(session.ID)
			emitRuntimeEvent(ctx, st, session, reqState, runtimeEventOptions{
				EventType:  observeNoticeEventType,
				ActionKind: "observe_mode",
				Decision:   stringPtr("notice"),
				Outcome:    stringPtr("injected"),
				Reason:     stringPtr("observe mode notice injected into the agent response stream"),
				Metadata: map[string]any{
					"delivery": "response_stream_injection",
					"provider": string(provider),
				},
			})
		}
	}
}

func injectResponseNoticesBody(req *http.Request, contentType string, body []byte, notices []responseNotice) ([]byte, bool) {
	if req == nil || len(body) == 0 || len(notices) == 0 || strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream") {
		return body, false
	}
	noticeText := joinResponseNoticeText(notices)
	switch {
	case conversation.MatchProviderAnthropic(req):
		return injectAnthropicResponseNoticeJSON(body, noticeText)
	case conversation.MatchProviderOpenAI(req):
		if conversation.IsOpenAIChatCompletionsEndpoint(req) {
			return injectOpenAIChatResponseNoticeJSON(body, noticeText)
		}
		return injectOpenAIResponsesNoticeJSON(body, noticeText)
	default:
		return body, false
	}
}

func (s *Server) tryStreamResponseNotices(req *http.Request, resp *http.Response, notices []responseNotice) bool {
	if req == nil || resp == nil || resp.Body == nil || len(notices) == 0 || !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return false
	}
	noticeText := joinResponseNoticeText(notices)
	switch {
	case conversation.MatchProviderAnthropic(req):
		resp.Body = newToolUseStreamBody(resp.Body, newAnthropicResponseNoticeStreamProcessor(noticeText))
		return true
	case conversation.MatchProviderOpenAI(req):
		if conversation.IsOpenAIChatCompletionsEndpoint(req) {
			resp.Body = newToolUseStreamBody(resp.Body, newOpenAIChatResponseNoticeStreamProcessor(noticeText))
			return true
		}
		resp.Body = newToolUseStreamBody(resp.Body, newOpenAIResponsesNoticeStreamProcessor(noticeText))
		return true
	default:
		return false
	}
}

func joinResponseNoticeText(notices []responseNotice) string {
	if len(notices) == 0 {
		return ""
	}
	parts := make([]string, 0, len(notices))
	for _, notice := range notices {
		text := strings.TrimSpace(notice.Text)
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func injectAnthropicResponseNoticeJSON(body []byte, noticeText string) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	content, ok := payload["content"].([]any)
	if !ok {
		return body, false
	}
	for i, item := range content {
		block, ok := item.(map[string]any)
		if !ok || block["type"] != "text" {
			continue
		}
		text, _ := block["text"].(string)
		block["text"] = prefixNoticeText(noticeText, text)
		content[i] = block
		payload["content"] = content
		rewritten, err := json.Marshal(payload)
		if err != nil {
			return body, false
		}
		return rewritten, true
	}
	payload["content"] = append([]any{map[string]any{
		"type": "text",
		"text": noticeText,
	}}, content...)
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

func injectOpenAIResponsesNoticeJSON(body []byte, noticeText string) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	output, ok := payload["output"].([]any)
	if !ok {
		return body, false
	}
	for i, item := range output {
		msg, ok := item.(map[string]any)
		if !ok || msg["type"] != "message" {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for j, part := range content {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := partMap["type"].(string)
			if partType != "output_text" && partType != "text" {
				continue
			}
			text, _ := partMap["text"].(string)
			partMap["text"] = prefixNoticeText(noticeText, text)
			content[j] = partMap
			msg["content"] = content
			output[i] = msg
			payload["output"] = output
			if outputText, ok := payload["output_text"].(string); ok {
				payload["output_text"] = prefixNoticeText(noticeText, outputText)
			}
			rewritten, err := json.Marshal(payload)
			if err != nil {
				return body, false
			}
			return rewritten, true
		}
	}
	payload["output"] = append([]any{map[string]any{
		"id":     "msg_clawvisor_notice",
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []map[string]any{{
			"type": "output_text",
			"text": noticeText,
		}},
	}}, output...)
	if outputText, ok := payload["output_text"].(string); ok {
		payload["output_text"] = prefixNoticeText(noticeText, outputText)
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

func injectOpenAIChatResponseNoticeJSON(body []byte, noticeText string) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return body, false
	}
	firstChoice, ok := choices[0].(map[string]any)
	if !ok {
		return body, false
	}
	message, ok := firstChoice["message"].(map[string]any)
	if !ok {
		return body, false
	}
	switch content := message["content"].(type) {
	case string:
		message["content"] = prefixNoticeText(noticeText, content)
	case []any:
		prefixed := false
		for i, part := range content {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := partMap["type"].(string)
			if partType != "text" && partType != "output_text" {
				continue
			}
			text, _ := partMap["text"].(string)
			partMap["text"] = prefixNoticeText(noticeText, text)
			content[i] = partMap
			prefixed = true
			break
		}
		if !prefixed {
			content = append([]any{map[string]any{"type": "text", "text": noticeText}}, content...)
		}
		message["content"] = content
	default:
		return body, false
	}
	firstChoice["message"] = message
	choices[0] = firstChoice
	payload["choices"] = choices
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

type anthropicResponseNoticeStreamProcessor struct {
	text      string
	injected  bool
	nextIndex int
}

func newAnthropicResponseNoticeStreamProcessor(text string) *anthropicResponseNoticeStreamProcessor {
	return &anthropicResponseNoticeStreamProcessor{text: text}
}

func (p *anthropicResponseNoticeStreamProcessor) ProcessBlock(raw []byte) ([]byte, bool, error) {
	event, data, ok := parseSSEBlock(raw)
	if !ok {
		return raw, false, nil
	}
	p.bumpAnthropicIndex(data)
	if !p.injected && event == "content_block_delta" {
		if rewritten, changed := prefixAnthropicTextDeltaBlock(raw, p.text); changed {
			p.injected = true
			return rewritten, false, nil
		}
	}
	if !p.injected && (event == "message_delta" || event == "message_stop") {
		p.injected = true
		return append(synthAnthropicNoticeBlock(p.nextIndex, p.text), raw...), false, nil
	}
	return raw, false, nil
}

func (p *anthropicResponseNoticeStreamProcessor) Finish() ([]byte, error) {
	if p.injected {
		return nil, nil
	}
	p.injected = true
	return synthAnthropicNoticeBlock(p.nextIndex, p.text), nil
}

func (p *anthropicResponseNoticeStreamProcessor) bumpAnthropicIndex(data string) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return
	}
	index, ok := intFromSSEAny(raw["index"])
	if !ok {
		return
	}
	if index >= p.nextIndex {
		p.nextIndex = index + 1
	}
}

type openAIResponsesNoticeStreamProcessor struct {
	text            string
	injected        bool
	nextOutputIndex int
}

func newOpenAIResponsesNoticeStreamProcessor(text string) *openAIResponsesNoticeStreamProcessor {
	return &openAIResponsesNoticeStreamProcessor{text: text}
}

func (p *openAIResponsesNoticeStreamProcessor) ProcessBlock(raw []byte) ([]byte, bool, error) {
	event, data, ok := parseSSEBlock(raw)
	if !ok {
		return raw, false, nil
	}
	p.bumpOutputIndex(data)
	if !p.injected && event == "response.output_text.delta" {
		if rewritten, changed := prefixOpenAIResponsesTextDeltaBlock(raw, p.text); changed {
			p.injected = true
			return rewritten, false, nil
		}
	}
	if !p.injected && event == "response.completed" {
		p.injected = true
		return append(synthOpenAIResponsesNoticeBlock(p.nextOutputIndex, p.text), raw...), false, nil
	}
	return raw, false, nil
}

func (p *openAIResponsesNoticeStreamProcessor) Finish() ([]byte, error) {
	if p.injected {
		return nil, nil
	}
	p.injected = true
	return synthOpenAIResponsesNoticeBlock(p.nextOutputIndex, p.text), nil
}

func (p *openAIResponsesNoticeStreamProcessor) bumpOutputIndex(data string) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return
	}
	index, ok := intFromSSEAny(raw["output_index"])
	if !ok {
		return
	}
	if index >= p.nextOutputIndex {
		p.nextOutputIndex = index + 1
	}
}

type openAIChatResponseNoticeStreamProcessor struct {
	text     string
	injected bool
}

func newOpenAIChatResponseNoticeStreamProcessor(text string) *openAIChatResponseNoticeStreamProcessor {
	return &openAIChatResponseNoticeStreamProcessor{text: text}
}

func (p *openAIChatResponseNoticeStreamProcessor) ProcessBlock(raw []byte) ([]byte, bool, error) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var payload string
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			break
		}
	}
	if payload == "" {
		return raw, false, nil
	}
	if payload == "[DONE]" {
		if p.injected {
			return raw, false, nil
		}
		p.injected = true
		return append(synthOpenAIChatNoticeBlock(p.text), raw...), false, nil
	}
	var msg struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		return raw, false, nil
	}
	if p.injected {
		return raw, false, nil
	}
	if rewritten, changed := prefixOpenAIChatTextDeltaBlock(raw, p.text); changed {
		p.injected = true
		return rewritten, false, nil
	}
	for _, choice := range msg.Choices {
		if strings.TrimSpace(choice.FinishReason) != "" {
			p.injected = true
			return append(synthOpenAIChatNoticeBlock(p.text), raw...), false, nil
		}
	}
	return raw, false, nil
}

func (p *openAIChatResponseNoticeStreamProcessor) Finish() ([]byte, error) {
	if p.injected {
		return nil, nil
	}
	p.injected = true
	return synthOpenAIChatNoticeBlock(p.text), nil
}

func intFromSSEAny(v any) (int, bool) {
	switch typed := v.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case json.Number:
		n, err := typed.Int64()
		return int(n), err == nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(typed))
		return n, err == nil
	default:
		return 0, false
	}
}

func synthAnthropicNoticeBlock(index int, text string) []byte {
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
	emit("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": index,
	})
	return b.Bytes()
}

func synthOpenAIResponsesNoticeBlock(outputIndex int, text string) []byte {
	var b strings.Builder
	b.WriteString(sseEventBlock("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": outputIndex,
		"item":         map[string]any{"id": "msg_clawvisor_notice", "type": "message", "role": "assistant", "status": "in_progress"},
	}))
	b.WriteString(sseEventBlock("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       "msg_clawvisor_notice",
		"output_index":  outputIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": ""},
	}))
	b.WriteString(sseEventBlock("response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       "msg_clawvisor_notice",
		"output_index":  outputIndex,
		"content_index": 0,
		"delta":         text,
	}))
	b.WriteString(sseEventBlock("response.output_text.done", map[string]any{
		"type":          "response.output_text.done",
		"item_id":       "msg_clawvisor_notice",
		"output_index":  outputIndex,
		"content_index": 0,
		"text":          text,
	}))
	b.WriteString(sseEventBlock("response.content_part.done", map[string]any{
		"type":          "response.content_part.done",
		"item_id":       "msg_clawvisor_notice",
		"output_index":  outputIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": text},
	}))
	b.WriteString(sseEventBlock("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": outputIndex,
		"item":         map[string]any{"id": "msg_clawvisor_notice", "type": "message", "role": "assistant", "status": "completed"},
	}))
	return []byte(b.String())
}

func synthOpenAIChatNoticeBlock(text string) []byte {
	return []byte(chatCompletionSSEBlock(map[string]any{
		"id":      "chatcmpl_clawvisor_notice",
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
	}))
}

func prefixNoticeText(noticeText, existing string) string {
	if strings.TrimSpace(existing) == "" {
		return noticeText
	}
	return noticeText + "\n\n" + existing
}

func prefixAnthropicTextDeltaBlock(raw []byte, noticeText string) ([]byte, bool) {
	event, data, ok := parseSSEBlock(raw)
	if !ok || event != "content_block_delta" {
		return raw, false
	}
	var msg struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(data), &msg); err != nil || msg.Delta.Type != "text_delta" {
		return raw, false
	}
	msg.Delta.Text = prefixNoticeText(noticeText, msg.Delta.Text)
	return anthropicSSEBlock(event, msg), true
}

func prefixOpenAIResponsesTextDeltaBlock(raw []byte, noticeText string) ([]byte, bool) {
	event, data, ok := parseSSEBlock(raw)
	if !ok || event != "response.output_text.delta" {
		return raw, false
	}
	var msg struct {
		Type         string `json:"type"`
		ItemID       string `json:"item_id"`
		OutputIndex  int    `json:"output_index"`
		ContentIndex int    `json:"content_index"`
		Delta        string `json:"delta"`
	}
	if err := json.Unmarshal([]byte(data), &msg); err != nil {
		return raw, false
	}
	msg.Delta = prefixNoticeText(noticeText, msg.Delta)
	return sseBlock(event, msg), true
}

func prefixOpenAIChatTextDeltaBlock(raw []byte, noticeText string) ([]byte, bool) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var payload string
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			break
		}
	}
	if payload == "" || payload == "[DONE]" {
		return raw, false
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		return raw, false
	}
	choices, ok := msg["choices"].([]any)
	if !ok {
		return raw, false
	}
	for i, choiceAny := range choices {
		choice, ok := choiceAny.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		content, _ := delta["content"].(string)
		if content == "" {
			continue
		}
		delta["content"] = prefixNoticeText(noticeText, content)
		choice["delta"] = delta
		choices[i] = choice
		msg["choices"] = choices
		return chatCompletionSSEMapBlock(msg), true
	}
	return raw, false
}

func anthropicSSEBlock(event string, data any) []byte {
	raw, _ := json.Marshal(data)
	return []byte("event: " + event + "\ndata: " + string(raw) + "\n\n")
}

func sseBlock(event string, data any) []byte {
	raw, _ := json.Marshal(data)
	return []byte("event: " + event + "\ndata: " + string(raw) + "\n\n")
}

func chatCompletionSSEMapBlock(data any) []byte {
	raw, _ := json.Marshal(data)
	return []byte("data: " + string(raw) + "\n\n")
}
