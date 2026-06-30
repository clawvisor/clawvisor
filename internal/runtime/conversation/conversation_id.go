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
// Returns "" only when the body has no user-authored turn at all (parse error,
// empty messages array, no user-role entry). For any request that carries a
// user message, ConversationID returns a non-empty id. Per-conversation
// isolation depends on this: a "" return used to mean "fall back to a shared
// (user, agent) bucket," which was the cross-conversation leak source.
//
// Decision order per provider (first non-empty wins):
//
//  1. Native session identifier on the wire (provider-specific).
//  2. Clawvisor-minted marker (`cv-conv-...`) echoed back in assistant
//     history. Minted by the handler on turn 1 when (1) is absent and
//     prepended to the first assistant response via
//     RenderAgentRoutingNotice; recovered here via FindInjectedConversationID.
//     The marker is the compaction-tolerant id: when paired with the
//     summarizer preservation directive in InjectControlNoticeWithSnapshot,
//     it survives summarizer-based compaction at >>fingerprint rate.
//  3. Fingerprint of the first user message — last-resort fallback for
//     pre-mint turns (rare race), mint failures (crypto/rand outage), or
//     harnesses that strip the marker before echoing.
//
// Native sources per provider:
//
//   - Anthropic (/v1/messages): body.metadata.user_id is a JSON-encoded
//     blob of the shape {device_id, account_uuid, session_id}. Returns
//     session_id. Stable per Claude Code session.
//
//   - OpenAI Responses (/v1/responses): body.prompt_cache_key, a UUID-
//     shaped value Codex sets so OpenAI's prompt cache matches across
//     turns. Stable per Codex session.
//
//   - OpenAI Chat Completions (/v1/chat/completions): no native session
//     identifier exists on the wire — marker / fingerprint only.
//
// The request shape is inspected without disturbing it — body is read-only.
func ConversationID(req *http.Request, provider Provider, body []byte) string {
	if len(body) == 0 {
		return ""
	}
	switch provider {
	case ProviderAnthropic:
		if id := anthropicNativeConversationID(body); id != "" {
			return id
		}
		if id := FindInjectedConversationID(req, ProviderAnthropic, body); id != "" {
			return id
		}
		return anthropicChatFingerprint(body)
	case ProviderOpenAI:
		if req != nil && isOpenAIChatCompletionsEndpoint(req) {
			if id := FindInjectedConversationID(req, ProviderOpenAI, body); id != "" {
				return id
			}
			return openAIChatFingerprint(body)
		}
		if id := openAIResponsesNativeConversationID(body); id != "" {
			return id
		}
		if id := FindInjectedConversationID(req, ProviderOpenAI, body); id != "" {
			return id
		}
		return openAIResponsesFingerprint(body)
	}
	return ""
}

// anthropicNativeConversationID returns the session id from
// metadata.user_id (Claude Code's convention) or "" when no native id
// is on the wire. Native-only — callers compose with marker echo and
// fingerprint via ConversationID().
func anthropicNativeConversationID(body []byte) string {
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
	// contains a JSON object. Tolerate either shape: nested object, or
	// a flat opaque string.
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

// openAIResponsesNativeConversationID returns prompt_cache_key (Codex's
// convention) or "" when no native id is on the wire.
func openAIResponsesNativeConversationID(body []byte) string {
	var probe struct {
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.PromptCacheKey)
}

// anthropicChatFingerprint hashes the first user message text from a
// /v1/messages body. Returns "" only when no user-role message with
// non-empty text content is present (parse error, empty messages
// array, assistant-only history). The "fp-" prefix matches
// openAIChatFingerprint's shape so audit consumers can recognize the
// id as a fingerprint regardless of provider.
func anthropicChatFingerprint(body []byte) string {
	var probe struct {
		Messages []anthropicMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	for _, msg := range probe.Messages {
		if msg.Role != "user" {
			continue
		}
		text := strings.TrimSpace(flattenAnthropicContent(msg.Content, 0))
		if text == "" {
			continue
		}
		sum := sha256.Sum256([]byte(text))
		return "fp-" + hex.EncodeToString(sum[:8])
	}
	return ""
}

// openAIResponsesFingerprint hashes the first user input item from a
// /v1/responses body. Tolerates both wire shapes the Responses API
// accepts: a top-level `input` string (the simplest shape), and an
// `input` array of role-tagged items.
func openAIResponsesFingerprint(body []byte) string {
	var probe struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || len(probe.Input) == 0 {
		return ""
	}
	// Shape 1: input is a bare string. The single user turn IS the input.
	var asString string
	if err := json.Unmarshal(probe.Input, &asString); err == nil {
		text := strings.TrimSpace(asString)
		if text == "" {
			return ""
		}
		sum := sha256.Sum256([]byte(text))
		return "fp-" + hex.EncodeToString(sum[:8])
	}
	// Shape 2: input is an array of items; find the first user-role item.
	var items []openAIInputItem
	if err := json.Unmarshal(probe.Input, &items); err != nil {
		return ""
	}
	for _, item := range items {
		if item.Role != "user" {
			continue
		}
		text := strings.TrimSpace(flattenOpenAIContent(item.Content))
		if text == "" {
			continue
		}
		sum := sha256.Sum256([]byte(text))
		return "fp-" + hex.EncodeToString(sum[:8])
	}
	return ""
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
