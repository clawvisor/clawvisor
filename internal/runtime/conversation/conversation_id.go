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
// user message, ConversationID returns a non-empty id — either the provider's
// native session identifier or, when that's absent, a fingerprint of the
// FIRST user message. Per-conversation isolation depends on this: a "" return
// used to mean "fall back to a shared (user, agent) bucket," which was the
// cross-conversation leak source. The fingerprint fallback gives every legacy
// client a stable per-conversation id without requiring a wire-protocol
// upgrade, so callers can treat "" as a hard signal that no conversation
// exists rather than as a routing hint.
//
// Identifier source per provider (first non-empty wins):
//
//   - Anthropic (/v1/messages):
//       1. body.metadata.user_id is a JSON-encoded blob of the shape
//          {device_id, account_uuid, session_id}. Returns session_id.
//          Stable per Claude Code session.
//       2. Fingerprint of the first user message — for raw Anthropic API
//          clients that don't set the Claude-Code-specific metadata.
//
//   - OpenAI Responses (/v1/responses):
//       1. body.prompt_cache_key, a UUID-shaped value Codex sets so OpenAI's
//          prompt cache matches across turns. Stable per Codex session.
//       2. Fingerprint of the first user input item — for generic OpenAI
//          Responses clients that don't set prompt_cache_key.
//
//   - OpenAI Chat Completions (/v1/chat/completions): no native session
//     identifier exists on the wire. First consult FindInjectedConversationID
//     for a Clawvisor-minted marker echoed back in assistant history. Fall
//     back to a fingerprint of the FIRST user message text when no marker
//     has been minted yet (or the harness stripped it). The first user
//     message is the most stable thing harnesses leave alone — system
//     prompts can be rewritten on policy changes, and compaction replaces
//     middle messages, but the first user-typed turn survives. Two
//     conversations starting with literally identical text will collide
//     under the fingerprint; the marker, when echoed, partitions cleanly.
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
			// Echoed Clawvisor marker is conclusive when present — it
			// was minted on turn 1 specifically because no native ID
			// exists, and the harness has now round-tripped it.
			if id := FindInjectedConversationID(req, ProviderOpenAI, body); id != "" {
				return id
			}
			// Pre-mint or marker-stripped fallback. Will be removed in
			// a follow-up once telemetry confirms the marker round-trips
			// reliably across the active OpenClaw harness population.
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
	if raw != "" {
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
	}
	return anthropicChatFingerprint(body)
}

func openAIResponsesConversationID(body []byte) string {
	var probe struct {
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		if key := strings.TrimSpace(probe.PromptCacheKey); key != "" {
			return key
		}
	}
	return openAIResponsesFingerprint(body)
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
