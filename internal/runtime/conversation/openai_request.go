package conversation

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

var openAIApprovalReplyRE = regexp.MustCompile(`(?i)\b(approve|deny)\s+(cv-[a-z0-9]{12})\b`)
var openAIBareApprovalRE = regexp.MustCompile(`(?i)^\s*(approve|deny)\s*$`)

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
	for i := len(probe.Messages) - 1; i >= 0; i-- {
		if probe.Messages[i].Role != "user" {
			continue
		}
		return parseApprovalReplyText(flattenOpenAIContent(probe.Messages[i].Content))
	}
	if len(probe.Input) > 0 {
		var items []openAIInputItem
		if err := json.Unmarshal(probe.Input, &items); err == nil {
			for i := len(items) - 1; i >= 0; i-- {
				if items[i].Type != "message" || items[i].Role != "user" {
					continue
				}
				return parseApprovalReplyText(flattenOpenAIContent(items[i].Content))
			}
		}
	}
	return "", ""
}

func parseApprovalReplyText(text string) (verb, id string) {
	text = strings.TrimSpace(text)
	if match := openAIApprovalReplyRE.FindStringSubmatch(text); match != nil {
		return strings.ToLower(match[1]), strings.ToLower(match[2])
	}
	if match := openAIBareApprovalRE.FindStringSubmatch(text); match != nil {
		return strings.ToLower(match[1]), ""
	}
	return "", ""
}

func isOpenAIResponsesEndpoint(req *http.Request) bool {
	return req != nil && req.URL != nil && (strings.HasPrefix(req.URL.Path, "/v1/responses") || strings.HasPrefix(req.URL.Path, "/backend-api/codex/responses"))
}

func isOpenAIChatCompletionsEndpoint(req *http.Request) bool {
	return req != nil && req.URL != nil && strings.HasPrefix(req.URL.Path, "/v1/chat/completions")
}

func IsOpenAIChatCompletionsEndpoint(req *http.Request) bool {
	return isOpenAIChatCompletionsEndpoint(req)
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
