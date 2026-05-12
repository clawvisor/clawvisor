package llmproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type TaskReplyRewriteRequest struct {
	HTTPRequest     *http.Request
	Provider        conversation.Provider
	Body            []byte
	Agent           *store.Agent
	PendingApproval PendingApprovalCache
}

type TaskReplyRewriteResult struct {
	Body      []byte
	Rewritten bool
}

func RewriteTaskApprovalReply(ctx context.Context, req TaskReplyRewriteRequest) (TaskReplyRewriteResult, error) {
	verb, approvalID := conversation.ApprovalReplyForProvider(req.Provider, req.Body)
	if verb != "task" || req.PendingApproval == nil || req.Agent == nil {
		return TaskReplyRewriteResult{Body: req.Body}, nil
	}
	pending, err := req.PendingApproval.Peek(ctx, ResolveRequest{
		UserID:     req.Agent.UserID,
		AgentID:    req.Agent.ID,
		Provider:   req.Provider,
		ApprovalID: approvalID,
	})
	if err != nil || pending == nil {
		return TaskReplyRewriteResult{Body: req.Body}, err
	}
	rewritten, ok, err := replaceTaskReplyForProvider(req.HTTPRequest, req.Provider, req.Body, taskCreationPrompt(pending.ToolUse))
	if err != nil || !ok {
		return TaskReplyRewriteResult{Body: req.Body}, err
	}
	return TaskReplyRewriteResult{Body: rewritten, Rewritten: true}, nil
}

func replaceTaskReplyForProvider(req *http.Request, provider conversation.Provider, body []byte, replacement string) ([]byte, bool, error) {
	switch provider {
	case conversation.ProviderAnthropic:
		return replaceAnthropicTaskReply(body, replacement)
	case conversation.ProviderOpenAI:
		if conversation.IsOpenAIChatCompletionsEndpoint(req) {
			return replaceOpenAIChatTaskReply(body, replacement)
		}
		return replaceOpenAIResponsesTaskReply(body, replacement)
	default:
		return body, false, nil
	}
}

func replaceAnthropicTaskReply(body []byte, replacement string) ([]byte, bool, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false, err
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		verb, _ := conversation.ParseApprovalReplyText(flattenAnthropicTaskReplyText(req.Messages[i].Content))
		if verb != "task" {
			return body, false, nil
		}
		encoded, _ := json.Marshal(replacement)
		req.Messages[i].Content = encoded
		messages, err := json.Marshal(req.Messages)
		if err != nil {
			return nil, false, err
		}
		raw["messages"] = messages
		out, err := json.Marshal(raw)
		return out, err == nil, err
	}
	return body, false, nil
}

func flattenAnthropicTaskReplyText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var simple string
	if err := json.Unmarshal(raw, &simple); err == nil {
		return simple
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	// Concatenate every text-bearing block in order. Returning only
	// the last block was a false-negative when the task reply was
	// split across blocks or sat in an earlier block — the downstream
	// parser finds the latest matching verb anywhere in the combined
	// text.
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func replaceOpenAIChatTaskReply(body []byte, replacement string) ([]byte, bool, error) {
	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false, err
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		role, _ := req.Messages[i]["role"].(string)
		if role != "user" {
			continue
		}
		contentRaw, _ := json.Marshal(req.Messages[i]["content"])
		verb, _ := conversation.ParseApprovalReplyText(flattenOpenAITaskReplyContent(contentRaw))
		if verb != "task" {
			return body, false, nil
		}
		req.Messages[i]["content"] = replacement
		messages, err := json.Marshal(req.Messages)
		if err != nil {
			return nil, false, err
		}
		raw["messages"] = messages
		out, err := json.Marshal(raw)
		return out, err == nil, err
	}
	return body, false, nil
}

func replaceOpenAIResponsesTaskReply(body []byte, replacement string) ([]byte, bool, error) {
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Input) == 0 {
		return body, false, err
	}
	var inputString string
	if err := json.Unmarshal(req.Input, &inputString); err == nil {
		verb, _ := conversation.ParseApprovalReplyText(inputString)
		if verb != "task" {
			return body, false, nil
		}
		encoded, _ := json.Marshal(replacement)
		raw["input"] = encoded
		out, err := json.Marshal(raw)
		return out, err == nil, err
	}
	var items []map[string]any
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return body, false, nil
	}
	for i := len(items) - 1; i >= 0; i-- {
		typ, _ := items[i]["type"].(string)
		role, _ := items[i]["role"].(string)
		if typ != "message" || role != "user" {
			continue
		}
		contentRaw, _ := json.Marshal(items[i]["content"])
		verb, _ := conversation.ParseApprovalReplyText(flattenOpenAITaskReplyContent(contentRaw))
		if verb != "task" {
			return body, false, nil
		}
		items[i]["content"] = []map[string]any{{"type": "input_text", "text": replacement}}
		input, err := json.Marshal(items)
		if err != nil {
			return nil, false, err
		}
		raw["input"] = input
		out, err := json.Marshal(raw)
		return out, err == nil, err
	}
	return body, false, nil
}

func flattenOpenAITaskReplyContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var simple string
	if err := json.Unmarshal(raw, &simple); err == nil {
		return simple
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	// Concatenate every text-bearing block in order. Returning only the
	// last block was a false-negative when "approve cv-<id>" was split
	// across blocks (e.g. ["please ", "approve cv-abc"]) or when the
	// approve verb sat in an earlier block followed by trailing prose.
	// The downstream parser regex finds the latest matching verb+id
	// anywhere in the combined text.
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text", "input_text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}
