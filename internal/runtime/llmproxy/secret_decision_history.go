package llmproxy

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

type SecretDecisionHistoryStripRequest struct {
	Provider conversation.Provider
	Body     []byte
}

type SecretDecisionHistoryStripResult struct {
	Body     []byte
	Modified bool
}

func StripSecretDecisionHistory(req SecretDecisionHistoryStripRequest) (SecretDecisionHistoryStripResult, error) {
	if len(req.Body) == 0 || !strings.Contains(string(req.Body), SecretDecisionIDMarker) {
		return SecretDecisionHistoryStripResult{Body: req.Body}, nil
	}
	switch req.Provider {
	case conversation.ProviderAnthropic:
		return stripAnthropicSecretDecisionHistory(req.Body)
	case conversation.ProviderOpenAI:
		return stripOpenAISecretDecisionHistory(req.Body)
	default:
		return SecretDecisionHistoryStripResult{Body: req.Body}, nil
	}
}

func stripAnthropicSecretDecisionHistory(body []byte) (SecretDecisionHistoryStripResult, error) {
	// Byte fidelity: this strips entire messages by index but never
	// modifies a surviving message's content, so we can preserve each
	// survivor's bytes verbatim via []json.RawMessage.
	msgsStart, msgsEnd, ok := findJSONFieldValue(body, "messages")
	if !ok {
		return SecretDecisionHistoryStripResult{Body: body}, nil
	}
	messages, ok := flattenJSONArray(body[msgsStart:msgsEnd])
	if !ok {
		return SecretDecisionHistoryStripResult{Body: body}, nil
	}
	out, modified := stripSecretDecisionMessagesRaw(messages, func(msg json.RawMessage) (string, string) {
		role := extractMessageRole(msg)
		return role, flattenAnthropicTaskReplyText(extractMessageContent(msg))
	})
	if !modified {
		return SecretDecisionHistoryStripResult{Body: body}, nil
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return SecretDecisionHistoryStripResult{Body: body}, err
	}
	newBody, err := SetJSONField(body, "messages", encoded)
	if err != nil {
		return SecretDecisionHistoryStripResult{Body: body}, err
	}
	return SecretDecisionHistoryStripResult{Body: newBody, Modified: true}, nil
}

func stripOpenAISecretDecisionHistory(body []byte) (SecretDecisionHistoryStripResult, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return SecretDecisionHistoryStripResult{Body: body}, nil
	}
	modified := false
	if messagesRaw := raw["messages"]; len(messagesRaw) > 0 {
		var messages []map[string]json.RawMessage
		if err := json.Unmarshal(messagesRaw, &messages); err == nil {
			out, changed := stripSecretDecisionMessages(messages, func(msg map[string]json.RawMessage) (string, string) {
				var role string
				_ = json.Unmarshal(msg["role"], &role)
				rawContent, _ := json.Marshal(msg["content"])
				return role, flattenOpenAITaskReplyContent(rawContent)
			})
			if changed {
				encoded, err := json.Marshal(out)
				if err != nil {
					return SecretDecisionHistoryStripResult{Body: body}, err
				}
				raw["messages"] = encoded
				modified = true
			}
		}
	}
	if inputRaw := raw["input"]; len(inputRaw) > 0 {
		var input []map[string]json.RawMessage
		if err := json.Unmarshal(inputRaw, &input); err == nil {
			out, changed := stripSecretDecisionMessages(input, func(item map[string]json.RawMessage) (string, string) {
				var role string
				_ = json.Unmarshal(item["role"], &role)
				rawContent, _ := json.Marshal(item["content"])
				return role, flattenOpenAITaskReplyContent(rawContent)
			})
			if changed {
				encoded, err := json.Marshal(out)
				if err != nil {
					return SecretDecisionHistoryStripResult{Body: body}, err
				}
				raw["input"] = encoded
				modified = true
			}
		}
	}
	if !modified {
		return SecretDecisionHistoryStripResult{Body: body}, nil
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return SecretDecisionHistoryStripResult{Body: body}, err
	}
	return SecretDecisionHistoryStripResult{Body: out, Modified: true}, nil
}

func stripSecretDecisionMessages(messages []map[string]json.RawMessage, text func(map[string]json.RawMessage) (string, string)) ([]map[string]json.RawMessage, bool) {
	out := make([]map[string]json.RawMessage, 0, len(messages))
	modified := false
	skipDecisionIndex := -1
	for i := 0; i < len(messages); i++ {
		if i == skipDecisionIndex {
			modified = true
			continue
		}
		role, content := text(messages[i])
		if role == "assistant" && strings.Contains(content, SecretDecisionIDMarker) {
			modified = true
			for j := i + 1; j < len(messages); j++ {
				nextRole, nextContent := text(messages[j])
				if nextRole != "user" {
					continue
				}
				if ParseSecretDecisionReply(nextContent).Action != SecretDecisionNone {
					skipDecisionIndex = j
				}
				break
			}
			continue
		}
		out = append(out, messages[i])
	}
	return out, modified
}

// stripSecretDecisionMessagesRaw is the byte-faithful analogue of
// stripSecretDecisionMessages: messages are passed as raw bytes so
// surviving entries pass through verbatim.
func stripSecretDecisionMessagesRaw(messages []json.RawMessage, text func(json.RawMessage) (string, string)) ([]json.RawMessage, bool) {
	out := make([]json.RawMessage, 0, len(messages))
	modified := false
	skipDecisionIndex := -1
	for i := 0; i < len(messages); i++ {
		if i == skipDecisionIndex {
			modified = true
			continue
		}
		role, content := text(messages[i])
		if role == "assistant" && strings.Contains(content, SecretDecisionIDMarker) {
			modified = true
			for j := i + 1; j < len(messages); j++ {
				nextRole, nextContent := text(messages[j])
				if nextRole != "user" {
					continue
				}
				if ParseSecretDecisionReply(nextContent).Action != SecretDecisionNone {
					skipDecisionIndex = j
				}
				break
			}
			continue
		}
		out = append(out, messages[i])
	}
	return out, modified
}
