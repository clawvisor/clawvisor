package llmproxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// agentNoticeMaxNameRunes caps the agent display name embedded in the
// first-turn notice. The agent name is operator-controlled (not
// model-authored), but a runaway name from a misconfigured deployment
// would still dominate the assistant turn without this. Matches the
// defensive cap on [autoApproveUserNotice].
const agentNoticeMaxNameRunes = 80

// RenderAgentRoutingNotice returns the human-facing one-liner the
// handler prepends to the first assistant turn of a conversation so the
// user can see at a glance that the conversation is being routed
// through Clawvisor and which agent identity is in use. Empty / blank
// agent names render a name-less fallback rather than an awkward empty
// quote.
func RenderAgentRoutingNotice(agentName string) string {
	cleaned := strings.TrimSpace(agentName)
	cleaned = strings.ReplaceAll(cleaned, "\r", " ")
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	cleaned = truncateRunes(cleaned, agentNoticeMaxNameRunes)
	if cleaned == "" {
		return "[Clawvisor] Routing this conversation through Clawvisor."
	}
	return fmt.Sprintf("[Clawvisor] Routing this conversation through Clawvisor as agent %q.", cleaned)
}

// HasInboundAssistantTurn reports whether the inbound LLM request body
// already contains at least one assistant turn. The first-message
// notice fires only when this returns false — i.e. the very first
// upstream call of a fresh conversation. On a malformed body or an
// unrecognized provider shape, returns true (fail-safe: skip the
// notice rather than risk prepending it on every turn of a
// long-running conversation we couldn't parse).
func HasInboundAssistantTurn(provider conversation.Provider, body []byte) bool {
	if len(body) == 0 {
		return true
	}
	switch provider {
	case conversation.ProviderAnthropic:
		return anthropicInboundHasAssistant(body)
	case conversation.ProviderOpenAI:
		return openAIInboundHasAssistant(body)
	default:
		return true
	}
}

func anthropicInboundHasAssistant(body []byte) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return true
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return true
	}
	var messages []struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(msgsRaw, &messages); err != nil {
		return true
	}
	for _, m := range messages {
		if m.Role == "assistant" {
			return true
		}
	}
	return false
}

func openAIInboundHasAssistant(body []byte) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return true
	}
	// Chat Completions shape: messages[].role
	if msgsRaw, ok := raw["messages"]; ok {
		var messages []struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(msgsRaw, &messages); err != nil {
			return true
		}
		for _, m := range messages {
			if m.Role == "assistant" {
				return true
			}
		}
		return false
	}
	// Responses API shape: `input` is either a plain string (single
	// user turn, no prior assistant) or an array of typed items where
	// assistant turns appear as role:"assistant" message items.
	if inputRaw, ok := raw["input"]; ok {
		var asString string
		if err := json.Unmarshal(inputRaw, &asString); err == nil {
			return false
		}
		var items []struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(inputRaw, &items); err != nil {
			return true
		}
		for _, it := range items {
			if it.Type != "" && it.Type != "message" {
				continue
			}
			if it.Role == "assistant" {
				return true
			}
		}
		return false
	}
	// No messages and no input — fail safe.
	return true
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	count := 0
	cut := len(s)
	for i := range s {
		if count == max {
			cut = i
			break
		}
		count++
	}
	if cut == len(s) {
		return s
	}
	return s[:cut] + "…"
}
