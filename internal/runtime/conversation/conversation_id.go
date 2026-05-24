package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// ConversationID returns a stable per-conversation identifier derived from the
// agent's request body. It is used to scope per-agent state — pending approval
// holds, task checkout focus — to a single conversation, so that two agents
// sharing a Clawvisor token (Conductor workspaces, sub-agents, multiple chat
// sessions in the same harness) don't clobber each other's approvals or focus.
//
// Returns "" when no identifier can be derived. Callers must treat empty as
// "unknown conversation" and fall back to the pre-conversation-scoping
// behavior — empty IDs MUST collide rather than partition, otherwise old
// clients silently lose their previous-turn state on every request.
//
// Identifier source per provider:
//
//   - Anthropic (/v1/messages): body.metadata.user_id is a JSON-encoded blob
//     of the shape {device_id, account_uuid, session_id}. Returns session_id.
//     Stable per Claude Code session.
//
//   - OpenAI Responses (/v1/responses): body.prompt_cache_key, a UUID-shaped
//     value Codex sets so OpenAI's prompt cache matches across turns. Stable
//     per Codex session.
//
//   - OpenAI Chat Completions (/v1/chat/completions): no native session
//     identifier exists on the wire. Fall back to a fingerprint of the FIRST
//     user message text. The first user message is the most stable thing
//     harnesses leave alone — system prompts can be rewritten on policy
//     changes, and compaction replaces middle messages, but the first
//     user-typed turn survives. Two conversations starting with literally
//     identical text will collide; acceptable.
//
// The request shape is inspected without disturbing it — body is read-only.
func ConversationID(req *http.Request, provider Provider, body []byte) string {
	if len(body) == 0 {
		return ""
	}
	switch provider {
	case ProviderAnthropic:
		return anthropicConversationID(body)
	case ProviderOpenAI:
		if req != nil && isOpenAIChatCompletionsEndpoint(req) {
			return openAIChatFingerprint(body)
		}
		return openAIResponsesConversationID(body)
	}
	return ""
}

func anthropicConversationID(body []byte) string {
	var probe struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	raw := strings.TrimSpace(probe.Metadata.UserID)
	if raw == "" {
		return ""
	}
	// Claude Code encodes metadata.user_id as a JSON string that itself
	// contains a JSON object. Tolerate either shape: nested object, or a
	// flat opaque string.
	var nested struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(raw), &nested); err == nil {
		if id := strings.TrimSpace(nested.SessionID); id != "" {
			return id
		}
	}
	return ""
}

func openAIResponsesConversationID(body []byte) string {
	var probe struct {
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.PromptCacheKey)
}

func openAIChatFingerprint(body []byte) string {
	var probe struct {
		Messages []openAIMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	for _, msg := range probe.Messages {
		if msg.Role != "user" {
			continue
		}
		text := strings.TrimSpace(flattenOpenAIContent(msg.Content))
		if text == "" {
			continue
		}
		sum := sha256.Sum256([]byte(text))
		return "fp-" + hex.EncodeToString(sum[:8])
	}
	return ""
}
