package conversation

import (
	"encoding/json"
	"net/http"
	"strings"
)

func OpenAIRequestWantsStream(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream
}

func MatchProviderOpenAI(req *http.Request) bool {
	return matchOpenAIEndpoint(req)
}

func OpenAIToolResultIDsFromRequest(req *http.Request, body []byte) []string {
	if isOpenAIChatCompletionsEndpoint(req) {
		return openAIChatToolResultIDs(body)
	}
	return openAIResponsesToolResultIDs(body)
}

func OpenAIApprovalReply(body []byte) (verb, id string) {
	var probe struct {
		Messages []openAIMessage `json:"messages"`
		Input    json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", ""
	}
	userIdx := -1
	for i := len(probe.Messages) - 1; i >= 0; i-- {
		if probe.Messages[i].Role == "user" {
			userIdx = i
			break
		}
	}
	if userIdx >= 0 {
		verb, id = ParseApprovalReplyText(flattenOpenAIContent(probe.Messages[userIdx].Content))
		if verb == "" || id != "" {
			return verb, id
		}
		// Bare reply: scan back through assistant messages for the most
		// recent approval-ID marker so the reply pins to the specific
		// hold this transcript is looking at, rather than whatever
		// LIFO picks from the pending cache.
		for i := userIdx - 1; i >= 0; i-- {
			if probe.Messages[i].Role != "assistant" {
				continue
			}
			if marker := FindLatestApprovalIDMarker(flattenOpenAIContent(probe.Messages[i].Content)); marker != "" {
				return verb, marker
			}
		}
		return verb, ""
	}
	if len(probe.Input) > 0 {
		var items []openAIInputItem
		if err := json.Unmarshal(probe.Input, &items); err == nil {
			itemUserIdx := -1
			for i := len(items) - 1; i >= 0; i-- {
				if items[i].Type == "message" && items[i].Role == "user" {
					itemUserIdx = i
					break
				}
			}
			if itemUserIdx < 0 {
				return "", ""
			}
			verb, id = ParseApprovalReplyText(flattenOpenAIContent(items[itemUserIdx].Content))
			if verb == "" || id != "" {
				return verb, id
			}
			for i := itemUserIdx - 1; i >= 0; i-- {
				if items[i].Type != "message" || items[i].Role != "assistant" {
					continue
				}
				if marker := FindLatestApprovalIDMarker(flattenOpenAIContent(items[i].Content)); marker != "" {
					return verb, marker
				}
			}
			return verb, ""
		}
	}
	return "", ""
}

func isOpenAIResponsesEndpoint(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	path := liteProxyProviderPath(req.URL.Path)
	return strings.HasPrefix(path, "/v1/responses") || strings.HasPrefix(path, "/backend-api/codex/responses")
}

func isOpenAIChatCompletionsEndpoint(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	return strings.HasPrefix(liteProxyProviderPath(req.URL.Path), "/v1/chat/completions")
}

func IsOpenAIChatCompletionsEndpoint(req *http.Request) bool {
	return isOpenAIChatCompletionsEndpoint(req)
}

func liteProxyProviderPath(path string) string {
	if path == "/api" {
		return ""
	}
	if strings.HasPrefix(path, "/api/") {
		return strings.TrimPrefix(path, "/api")
	}
	return path
}

func openAIResponsesToolResultIDs(body []byte) []string {
	var probe struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || len(probe.Input) == 0 {
		return nil
	}
	var items []openAIInputItem
	if err := json.Unmarshal(probe.Input, &items); err != nil {
		return nil
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.Type == "function_call_output" && item.CallID != "" {
			ids = append(ids, item.CallID)
		}
	}
	return ids
}

func openAIChatToolResultIDs(body []byte) []string {
	var probe struct {
		Messages []openAIMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil
	}
	ids := make([]string, 0, len(probe.Messages))
	for _, msg := range probe.Messages {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			ids = append(ids, msg.ToolCallID)
		}
	}
	return ids
}
