package llmproxy

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// maxRecentHumanTurns bounds how many of the most recent genuine human
// turns the extractor returns. Conversation-based approval needs the
// user's actual instruction, not the whole transcript — three turns is
// enough to cover "user spread the ask across a couple short messages"
// without ballooning the assessor's prompt or its attack surface.
const maxRecentHumanTurns = 3

// ExtractHumanTurnsRequest carries the inbound request body plus provider
// shape so the extractor can pick the right walker.
type ExtractHumanTurnsRequest struct {
	Provider conversation.Provider
	Body     []byte
}

// ExtractRecentHumanTurns returns the most recent genuine human-authored
// chat turns from an inbound LLM request body, in chronological order
// (most recent last). "Genuine" means: role:"user", contains real text
// content (not just tool_result blocks), and is not a Clawvisor-internal
// approval reply (bare "yes" / "no" / "task" verbs that the user typed
// to drive the approval flow itself, not as a fresh instruction).
//
// This is the input to conversation-based auto-approval: the risk
// assessor compares these turns to the task scope and decides whether
// the user's prior message(s) unambiguously authorize the work. Treat
// the output as UNTRUSTED data — it can contain injection — and pass it
// to the assessor only for evaluation, never as instruction.
//
// Returns an empty slice (not nil) when no genuine human turns exist or
// the body fails to parse. The empty case is the signal callers use to
// set intent_match=unknown.
func ExtractRecentHumanTurns(req ExtractHumanTurnsRequest) []string {
	if len(req.Body) == 0 {
		return []string{}
	}
	switch req.Provider {
	case conversation.ProviderAnthropic:
		return extractAnthropicHumanTurns(req.Body)
	case conversation.ProviderOpenAI:
		return extractOpenAIHumanTurns(req.Body)
	default:
		return []string{}
	}
}

func extractAnthropicHumanTurns(body []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return []string{}
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return []string{}
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(msgsRaw, &messages); err != nil {
		return []string{}
	}
	turns := make([]string, 0, len(messages))
	for _, msg := range messages {
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		if role != "user" {
			continue
		}
		text := strings.TrimSpace(extractAnthropicHumanText(msg["content"]))
		if text == "" {
			continue
		}
		if isClawvisorInternalUserText(text) {
			continue
		}
		turns = append(turns, text)
	}
	return tailLimit(turns, maxRecentHumanTurns)
}

// extractAnthropicHumanText returns the human-authored portion of an
// Anthropic role:"user" content field, flattened to plain text. The
// helper's contract is "what did the human type, ignoring everything
// the harness or the proxy injected" — concretely, it skips two
// categories of non-human content:
//
//  1. Tool-result blocks (harness output replayed back to the model).
//  2. Proxy-emitted <clawvisor-notice> blocks (proxy-issued control
//     state — task-approved / task-denied / task-expired envelopes).
//
// Both filters live here so every consumer (recent-human-turn
// extraction, expired-task-notice anchoring, future intent-style
// inspectors) gets the same shape without each one re-deriving the
// "what counts as the user's authoring?" decision.
//
// Content can be either a plain string (older shape) or a
// heterogeneous block array (current shape with type:"text" /
// "tool_result" entries).
func extractAnthropicHumanText(contentRaw json.RawMessage) string {
	if len(contentRaw) == 0 {
		return ""
	}
	// Simple string form.
	var simple string
	if err := json.Unmarshal(contentRaw, &simple); err == nil {
		return simple
	}
	// Block array form. Anthropic accepts "text" and "input_text" on
	// the user side. Allowlist these and skip everything else — a
	// denylist (skip tool_result, accept anything else with a text
	// field) would let any future Anthropic block type, or a
	// misbehaving harness emitting a custom type with an attacker-
	// controlled `text` field, be classified as a genuine human turn
	// and fed to the auto-approve assessor as if the user said it.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" && b.Type != "input_text" {
			continue
		}
		if b.Text == "" {
			continue
		}
		// Filter (2): drop proxy-emitted Clawvisor notice blocks.
		// PrependExpiredTaskNoticeToLastUserMessage produces a
		// [notice_block, user_text_block] shape; without this skip,
		// the flattened text would lead with the notice envelope and
		// downstream isClawvisorInternalUserText's exact-shape match
		// (which expects the ENTIRE trimmed text to be one notice)
		// would reject the prefix-followed-by-text combination,
		// letting the augmented turn surface as a "genuine human
		// turn" in auto-approval / intent-verify policies.
		if isExactClawvisorNoticeShape(b.Text) {
			continue
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

func extractOpenAIHumanTurns(body []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return []string{}
	}
	turns := make([]string, 0, 8)
	// Chat Completions shape: messages[].role + content.
	if msgsRaw, ok := raw["messages"]; ok {
		var messages []map[string]json.RawMessage
		if err := json.Unmarshal(msgsRaw, &messages); err == nil {
			for _, msg := range messages {
				var role string
				_ = json.Unmarshal(msg["role"], &role)
				if role != "user" {
					continue
				}
				text := strings.TrimSpace(flattenOpenAIChatContent(msg["content"]))
				if text == "" || isClawvisorInternalUserText(text) {
					continue
				}
				turns = append(turns, text)
			}
		}
	}
	// Responses API shape: `input` is either a flat array of items
	// (typed entries that include role:"user" messages) OR a plain
	// JSON string carrying the single user turn (the convenience form
	// the OpenAI Responses API accepts when there's no prior history,
	// e.g. {"input":"run echo"}). Try the string form first because
	// it's the cheap case; fall through to the array walk otherwise.
	if inputRaw, ok := raw["input"]; ok {
		var asString string
		if err := json.Unmarshal(inputRaw, &asString); err == nil {
			text := strings.TrimSpace(asString)
			if text != "" && !isClawvisorInternalUserText(text) {
				turns = append(turns, text)
			}
		} else {
			var items []map[string]json.RawMessage
			if err := json.Unmarshal(inputRaw, &items); err == nil {
				for _, item := range items {
					var typ string
					_ = json.Unmarshal(item["type"], &typ)
					if typ != "" && typ != "message" {
						continue
					}
					var role string
					_ = json.Unmarshal(item["role"], &role)
					if role != "user" {
						continue
					}
					text := strings.TrimSpace(flattenOpenAIChatContent(item["content"]))
					if text == "" || isClawvisorInternalUserText(text) {
						continue
					}
					turns = append(turns, text)
				}
			}
		}
	}
	return tailLimit(turns, maxRecentHumanTurns)
}

// flattenOpenAIChatContent handles both the string-content and block-
// array shapes the OpenAI Chat Completions / Responses APIs accept on
// the user side. Tool-call references on the user side don't exist in
// OpenAI's shape (those are role:"tool" messages, which we ignore by
// role filter), so we don't need a tool_result skip here.
func flattenOpenAIChatContent(raw json.RawMessage) string {
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
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// isClawvisorInternalUserText reports whether a user-role text block is
// actually a Clawvisor-internal artifact rather than a fresh human
// instruction. Two cases:
//
//  1. Bare approval verbs ("yes", "no", "y", "n", "task", or the
//     id-bound forms) — the user typed these to drive the approval
//     flow itself; they are not authorization for the underlying work.
//  2. Augmented inline-approval replies — the proxy inserts marker
//     text alongside the user's verb to disambiguate which pending
//     hold they meant; the augmentation should not be mistaken for an
//     authoring turn.
//
// Tool results and other non-human content are already filtered at the
// content-shape level (see extractAnthropicHumanText), so this helper
// only needs to recognize the conversational-verb cases.
func isClawvisorInternalUserText(text string) bool {
	// containsInlineApprovalAugmentationMarker catches the user-side
	// task-approved / task-denied / task-error notices the proxy
	// substitutes when an inline approval resolves. It looks for the
	// `<clawvisor-notice kind="task-` literal anywhere in the text —
	// since the augmenter REPLACES the user's verb with the notice
	// (not appends), an inbound user turn containing this substring
	// is by construction the proxy's own emission.
	if containsInlineApprovalAugmentationMarker(text) {
		return true
	}
	// Strict-shape match for any proxy-emitted <clawvisor-notice>
	// element (routing-style minted markers, future user-role notices).
	// Substring matching would be a footgun — a user asking "what is
	// <clawvisor-notice>?" would have their message silently dropped.
	if isExactClawvisorNoticeShape(text) {
		return true
	}
	// Only filter when the user's ENTIRE trimmed message is a bare
	// approval verb (or an id-bound verb). A multi-line genuine
	// instruction whose last line happens to end in "yes" is the
	// user authorizing their own request, not a Clawvisor-internal
	// reply to a pending hold. Without this narrowing,
	// "Please proceed with my plan.\n\nyes" was dropped entirely
	// from the auto-approve assessor's view and the deterministic
	// floor forced the manual prompt.
	return isExactApprovalReplyShape(text)
}

// exactClawvisorNoticeRE matches the entire string as a single
// well-formed <clawvisor-notice kind="..."> element. Kind is constrained
// to the same alphabet as the renderer's validator (`^[a-z0-9-]+$`);
// the body is forbidden from containing `<` so a body that holds a
// forged extra `</clawvisor-notice>` substring cannot trick the
// matcher into accepting trailing user text. The renderer XML-escapes
// `<` in bodies, so this restriction never rejects a genuine proxy-
// emitted notice.
var exactClawvisorNoticeRE = regexp.MustCompile(`^<clawvisor-notice kind="[a-z0-9-]+">[^<]*</clawvisor-notice>$`)

// isExactClawvisorNoticeShape reports whether the entire trimmed text
// is exactly one well-formed Clawvisor notice element. Trailing or
// leading content makes it NOT a match — a user-role message that
// merely mentions the tag (e.g. a question about it) stays a fresh
// human turn.
func isExactClawvisorNoticeShape(text string) bool {
	return exactClawvisorNoticeRE.MatchString(strings.TrimSpace(text))
}

// isExactApprovalReplyShape reports whether the supplied text, when
// trimmed of surrounding whitespace, is shaped exactly like an
// approval reply — either a bare verb ("yes", "no", "task", "y",
// "n", "approve", "deny") or a verb followed by a single
// cv-<id> token. Multi-line text and text with content beyond the
// verb (e.g. trailing prose) do NOT match.
func isExactApprovalReplyShape(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	// Reject any text that spans multiple non-empty lines — the
	// approval reply shape is single-line by definition. We count
	// non-empty lines rather than just checking for "\n" so a reply
	// with trailing blank lines still matches.
	nonEmptyLines := 0
	for _, line := range strings.Split(trimmed, "\n") {
		if strings.TrimSpace(line) != "" {
			nonEmptyLines++
			if nonEmptyLines > 1 {
				return false
			}
		}
	}
	// Run the parser on JUST the trimmed text. It already enforces
	// the single-line shape via its regex anchors (^$), so a match
	// here implies the entire trimmed input is a bare verb / id-bound
	// verb with no extra content. ParseApprovalReplyText scans
	// bottom-up, but on a single-line input that's equivalent to
	// matching the whole input.
	verb, _ := conversation.ParseApprovalReplyText(trimmed)
	return verb != ""
}

// tailLimit returns at most n trailing entries from in (most recent
// last). When in has <= n entries it's returned as-is. Returns a fresh
// slice so callers can't mutate the underlying buffer.
func tailLimit(in []string, n int) []string {
	if n <= 0 || len(in) == 0 {
		return []string{}
	}
	if len(in) <= n {
		out := make([]string, len(in))
		copy(out, in)
		return out
	}
	out := make([]string, n)
	copy(out, in[len(in)-n:])
	return out
}
