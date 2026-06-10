package llmproxy

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// AskUserQuestion-aware approval parsing for Anthropic bodies. The
// generic verb parser (conversation.AnthropicApprovalReply) handles
// the text-reply path; this file handles the case where the inline-
// approval intercept substituted an AskUserQuestion picker and the
// user's choice came back as a tool_result block instead of free
// text. Lives in llmproxy so the conversation package stays
// harness-agnostic — it doesn't need to know that "AskUserQuestion"
// is a specific Claude Code harness tool.

// anthropicMessage is the minimal Anthropic message shape both the
// detector and the body editor decode against. Local to this file
// so the helpers can be called without re-decoding into ad-hoc
// anonymous structs at each call site.
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicAskUserQuestionApprovalReply scans the latest user turn
// of an Anthropic body for a tool_result whose parent assistant
// tool_use is an AskUserQuestion call carrying the
// [clawvisor:approval=...] marker. Returns the user's normalized
// approve/deny verb plus the marker, or ("", "") when no matching
// pair exists. Callers fall through to the text-path result.
func anthropicAskUserQuestionApprovalReply(body []byte) (verb, id string) {
	var req struct {
		Messages []anthropicMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", ""
	}
	match, ok := findAnthropicAskUserQuestionMatch(req.Messages)
	if !ok {
		return "", ""
	}
	return match.Verb, match.ApprovalID
}

// anthropicAskUserQuestionMatch is the structured result of pairing
// the latest user-turn AskUserQuestion tool_result with its parent
// assistant tool_use and the [clawvisor:approval=...] marker in
// that turn. Both the read-only detector and the body editor
// consume this — keeping one finder so detection and rewrite never
// drift on which tool_result counts as the answer.
type anthropicAskUserQuestionMatch struct {
	UserIdx    int    // index of the user message holding the tool_result
	ToolUseID  string // tool_use_id of the answered AskUserQuestion call
	Verb       string // normalized approve/deny verb
	ApprovalID string // [clawvisor:approval=cv-...] marker the question carried
}

// findAnthropicAskUserQuestionMatch performs the shared finding
// logic used by both anthropicAskUserQuestionApprovalReply (detect-
// only) and replaceAnthropicAskUserQuestionApprovalReply (rewrite).
// Returns (match, true) on a successful pair or (_, false) when no
// AskUserQuestion-shaped approval reply exists in the body.
func findAnthropicAskUserQuestionMatch(messages []anthropicMessage) (anthropicAskUserQuestionMatch, bool) {
	userIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			userIdx = i
			break
		}
	}
	if userIdx < 0 {
		return anthropicAskUserQuestionMatch{}, false
	}
	results := extractAnthropicToolResults(messages[userIdx].Content)
	if len(results) == 0 {
		return anthropicAskUserQuestionMatch{}, false
	}
	questions := collectAnthropicAskUserQuestionInputs(messages, userIdx)
	if len(questions) == 0 {
		return anthropicAskUserQuestionMatch{}, false
	}
	// Walk tool_results in reverse so the latest answer wins when
	// the same turn carries multiple tool_results.
	for i := len(results) - 1; i >= 0; i-- {
		tr := results[i]
		questionText, ok := questions[tr.ToolUseID]
		if !ok {
			continue
		}
		marker := conversation.FindLatestApprovalIDMarker(questionText)
		if marker == "" {
			continue
		}
		// AskUserQuestion harnesses wrap the user's choice. Claude
		// Code emits `Your questions have been answered: "<q>"="<a>".
		// You can now continue with these answers in mind.` — the
		// raw answer label sits between the trailing `="..."` pair.
		// Strip the wrapper before handing the content to the bare-
		// verb parser, which only matches single-line yes/no/etc.
		answer := extractAskUserQuestionAnswer(tr.Content)
		if answer == "" {
			// Some harnesses pass the answer through unwrapped;
			// fall back to the generic parser so a plain "yes"
			// tool_result content still releases the hold.
			answer = tr.Content
		}
		v, _ := conversation.ParseApprovalReplyText(answer)
		if v == "" {
			continue
		}
		return anthropicAskUserQuestionMatch{
			UserIdx:    userIdx,
			ToolUseID:  tr.ToolUseID,
			Verb:       v,
			ApprovalID: marker,
		}, true
	}
	return anthropicAskUserQuestionMatch{}, false
}

// askUserQuestionAnswerRE pulls the answer label out of the harness-
// wrapped AskUserQuestion tool_result content. The leading question
// text is unbounded (spans newlines, contains arbitrary characters);
// the answer is what follows the trailing `"="` separator and lives
// between the next pair of double quotes. Non-greedy `[^"]*` on the
// answer keeps the match anchored to the rightmost answer slot when
// the wrapper trailer omits the period (older harness builds).
var askUserQuestionAnswerRE = regexp.MustCompile(`"="([^"]*)"`)

// extractAskUserQuestionAnswer returns the answer label embedded in
// a harness-wrapped AskUserQuestion result. Returns "" when the
// content doesn't match the wrapper shape — callers fall through to
// the generic verb parser on the raw content. Exported for test
// reuse via ExtractAskUserQuestionAnswer.
func extractAskUserQuestionAnswer(content string) string {
	matches := askUserQuestionAnswerRE.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return ""
	}
	// Last match handles multi-question results (each question gets
	// its own `"="..."` slot). For an inline-approval prompt we
	// only emit a single question, so the last match equals the
	// only match in production; the slice walk just makes the
	// contract explicit for future multi-question reuse.
	return matches[len(matches)-1][1]
}

// ExtractAskUserQuestionAnswer is the exported entry point sibling
// packages call so they share the same wrapper-unwrap logic as the
// release path.
func ExtractAskUserQuestionAnswer(content string) string {
	return extractAskUserQuestionAnswer(content)
}

// anthropicToolResult is the flattened (tool_use_id, text content)
// pair the AskUserQuestion detector and body editor operate on.
type anthropicToolResult struct {
	ToolUseID string
	Content   string
}

// extractAnthropicToolResults pulls tool_result blocks out of an
// Anthropic role:"user" content field, flattening any text-shaped
// content to a single string per block. Non-text content (image,
// etc.) is skipped — the AskUserQuestion answer is always text.
func extractAnthropicToolResults(raw json.RawMessage) []anthropicToolResult {
	if len(raw) == 0 {
		return nil
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	var out []anthropicToolResult
	for _, b := range blocks {
		if b.Type != "tool_result" || b.ToolUseID == "" {
			continue
		}
		content := flattenAnthropicToolResultContent(b.Content)
		if content == "" {
			continue
		}
		out = append(out, anthropicToolResult{ToolUseID: b.ToolUseID, Content: content})
	}
	return out
}

// flattenAnthropicToolResultContent accepts the two shapes
// Anthropic's tool_result.content field permits — a bare string or
// an array of typed blocks — and returns the concatenated text.
// JSON-typed answer payloads (e.g. {"answers":{"...":"yes"}})
// flatten to their raw JSON so the verb parser can still pick out
// "yes"/"no" tokens.
func flattenAnthropicToolResultContent(raw json.RawMessage) string {
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
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	// Fallback: return the raw JSON so downstream verb parsing can
	// pick "yes"/"no" out of a structured answer payload the harness
	// emits as an object rather than a string.
	return string(raw)
}

// collectAnthropicAskUserQuestionInputs walks every assistant
// message before userIdx and returns a map of AskUserQuestion
// tool_use IDs to the marker-search text for that specific call:
// the AskUserQuestion input JSON plus the text blocks PRECEDING it
// in the same assistant message (up to the previous
// AskUserQuestion).
//
// Per-call pairing matters when an assistant turn contains multiple
// AskUserQuestion calls — the inline-approval renderer emits each
// call right after its own [clawvisor:approval=...] marker text, so
// the marker for one call must NOT bleed into the search text of a
// later call. Without this scoping, the rewrite could land on the
// wrong tool_result and a chain of mixed-up approval replacements.
//
// Cross-conversation safety: callers gate by approvalID before
// consuming the resolved hold, so a marker captured here can't
// release a hold from a different conversation.
//
// Shared between anthropicAskUserQuestionApprovalReply (read-only
// detection) and replaceAnthropicAskUserQuestionApprovalReply (body
// rewrite) so the two never drift on what counts as a per-call
// marker-search string.
func collectAnthropicAskUserQuestionInputs(messages []anthropicMessage, userIdx int) map[string]string {
	out := make(map[string]string)
	for i := 0; i < userIdx; i++ {
		if messages[i].Role != "assistant" {
			continue
		}
		var blocks []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Text  string          `json:"text"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(messages[i].Content, &blocks); err != nil {
			continue
		}
		var pendingTexts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				pendingTexts = append(pendingTexts, b.Text)
				continue
			}
			if b.Type != "tool_use" || b.Name != AskUserQuestionToolName || b.ID == "" {
				continue
			}
			text := ""
			if len(b.Input) > 0 {
				text = string(b.Input)
			}
			if len(pendingTexts) > 0 {
				preceding := strings.Join(pendingTexts, "\n")
				if text != "" {
					text += "\n" + preceding
				} else {
					text = preceding
				}
			}
			if text != "" {
				out[b.ID] = text
			}
			pendingTexts = pendingTexts[:0]
		}
	}
	return out
}
