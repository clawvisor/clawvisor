package llmproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// agentNoticeMaxNameRunes caps the agent display name embedded in the
// first-turn notice. The agent name is operator-controlled (not
// model-authored), but a runaway name from a misconfigured deployment
// would still dominate the assistant turn without this. Matches the
// defensive cap on [AutoApproveUserNotice].
const agentNoticeMaxNameRunes = 80

// agentNoticeMaxBrandRunes caps the brand label embedded in the
// first-turn notice prose. Operator-controlled like the agent name;
// the cap is purely defensive against a misconfigured deployment.
const agentNoticeMaxBrandRunes = 40

// defaultAgentNoticeBrand is the fallback brand label used when the
// caller passes an empty / whitespace-only brandLabel.
const defaultAgentNoticeBrand = "Clawvisor"

// RenderAgentRoutingNotice returns the human-facing one-liner the
// handler prepends to the first assistant turn of a conversation so the
// user can see at a glance that the conversation is being routed
// through Clawvisor and which agent identity is in use. Empty / blank
// agent names render a name-less fallback rather than an awkward empty
// quote.
//
// brandLabel is the environment-qualified product name surfaced in the
// prose ("Clawvisor Local", "Clawvisor Staging", "Clawvisor"). Empty /
// whitespace falls back to plain "Clawvisor". The leading `[Clawvisor]`
// tag is intentionally not parameterized — it is a stable subsystem
// identifier, not a brand surface.
//
// mintedConversationID, when non-empty, is appended as a parseable
// [clawvisor:conversation=cv-conv-…] footer so the harness round-trips
// the ID back to us in assistant history on turn 2+. Only set on
// harnesses without a native session identifier (today: OpenAI Chat
// Completions); empty for Anthropic / OpenAI Responses where the
// native session ID is the conclusive scope key.
func RenderAgentRoutingNotice(agentName, mintedConversationID, brandLabel string) string {
	cleaned := strings.TrimSpace(agentName)
	cleaned = strings.ReplaceAll(cleaned, "\r", " ")
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	// Strip backticks so a name with one can't end the markdown
	// inline-code span the notice is wrapped in. The agent name is
	// operator-controlled (low risk in practice) but the cost of
	// dropping a stray backtick is nil.
	cleaned = strings.ReplaceAll(cleaned, "`", "")
	cleaned = truncateRunes(cleaned, agentNoticeMaxNameRunes)
	brand := sanitizeBrandLabel(brandLabel)
	// Backticks wrap the proxy status line so it renders as inline
	// code in markdown UIs — visually distinct from the assistant's
	// own prose. This notice lands in the assistant turn (human-facing),
	// so the prefix shape is `[Clawvisor]` rather than the structured
	// `<clawvisor-notice>` tag, which is reserved for user-role
	// injections the LLM reads.
	var notice string
	if cleaned == "" {
		notice = fmt.Sprintf("`[Clawvisor] Routing this conversation through %s.`", brand)
	} else {
		notice = fmt.Sprintf("`[Clawvisor] Routing this conversation through %s as agent %q.`", brand, cleaned)
	}
	if id := strings.TrimSpace(mintedConversationID); id != "" {
		notice += " " + conversation.RenderConversationIDMarker(id)
	}
	return notice
}

// sanitizeBrandLabel normalizes the brand label the way the agent name
// is normalized — strip CR/LF and backticks so neither breaks the
// markdown inline-code span, cap the rune length, and fall back to
// "Clawvisor" when the result is empty.
func sanitizeBrandLabel(raw string) string {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.ReplaceAll(cleaned, "\r", " ")
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	cleaned = strings.ReplaceAll(cleaned, "`", "")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return defaultAgentNoticeBrand
	}
	return truncateRunes(cleaned, agentNoticeMaxBrandRunes)
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
	// Responses API stateful chaining: `previous_response_id`
	// back-references a prior response, which is always assistant
	// output. Conclusive evidence of prior history regardless of what
	// `input` carries — check before walking `input`.
	//
	// We deliberately do NOT treat the top-level `conversation` field
	// the same way. A conversation is a container that can be brand-
	// new and empty on a fresh client kickoff, so its presence alone
	// is not evidence that any assistant turn has occurred yet.
	// Clients that chain turns within a conversation typically also
	// set `previous_response_id` (the conversation field carries
	// state; the response_id chain carries the back-reference), so
	// the conclusive signal is still available. A client that chains
	// via `conversation` ALONE and ships only a new user turn each
	// time will get the notice re-prepended on every turn — degenerate
	// but acceptable for a UX-polish surface.
	if prev, ok := raw["previous_response_id"]; ok && hasNonEmptyJSONStringOrObject(prev) {
		return true
	}
	// Responses API shape: `input` is either a plain string (single
	// user turn, no prior assistant) or an array of typed items. The
	// "no prior assistant" set is small and well-known: `message` items
	// with role in {user, system, developer}. Everything else carries
	// assistant-side state — assistant-role messages, `function_call`,
	// `custom_tool_call`, `reasoning`, built-in tool calls
	// (`web_search_call`, `file_search_call`, …), and even
	// `function_call_output` (which only exists in response to a prior
	// assistant function_call). Enumerate the user-side shape and treat
	// everything else as assistant history; otherwise a turn-2+ request
	// whose assistant turn was a tool call (no text message) would be
	// misread as the first turn and re-prepended on every continuation.
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
			// Default item type is "message" when omitted.
			typ := it.Type
			if typ == "" {
				typ = "message"
			}
			if typ != "message" {
				return true
			}
			switch it.Role {
			case "user", "system", "developer":
				continue
			default:
				return true
			}
		}
		return false
	}
	// No messages and no input — fail safe.
	return true
}

// hasNonEmptyJSONStringOrObject reports whether the raw JSON value is a
// non-empty string, a non-null object, or any other concrete (non-null,
// non-empty-string) value. Used to detect whether an optional pointer-
// like field on the Responses request (`previous_response_id`,
// `conversation`) is actually set vs. literally `null` / `""` / absent.
func hasNonEmptyJSONStringOrObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	if bytes.Equal(trimmed, []byte(`""`)) {
		return false
	}
	// Try as string first — the common case for previous_response_id
	// ("resp_abc123") and the string form of conversation
	// ("conv_xyz789").
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		return strings.TrimSpace(s) != ""
	}
	// Object form (e.g. {"id":"conv_xyz789"}) — anything that parses
	// as a non-null object counts.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err == nil {
		return len(obj) > 0
	}
	return false
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
