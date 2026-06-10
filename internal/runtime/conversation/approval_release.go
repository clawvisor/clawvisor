package conversation

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

const ApprovalDeniedMessage = "Approval denied. The requested tool call was not performed."

type SyntheticApprovalResponse struct {
	ContentType string
	Body        []byte
}

func ApprovalReplyForProvider(provider Provider, body []byte) (verb, id string) {
	switch provider {
	case ProviderAnthropic:
		return AnthropicApprovalReply(body)
	case ProviderOpenAI:
		return OpenAIApprovalReply(body)
	default:
		return "", ""
	}
}

func AnthropicApprovalReply(body []byte) (verb, id string) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", ""
	}
	userIdx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			userIdx = i
			break
		}
	}
	if userIdx < 0 {
		return "", ""
	}
	verb, id = ParseApprovalReplyText(flattenAnthropicUserText(req.Messages[userIdx].Content))
	if verb != "" {
		if id == "" {
			// Bare reply (e.g. "y"): scan back through assistant messages for the
			// most recent approval-ID marker. Without this, a "y" lands on whatever
			// hold the cache picks LIFO — which can be the wrong agent's when
			// several share a Clawvisor token. The marker pins the reply to the
			// specific approval prompt this transcript is looking at.
			for i := userIdx - 1; i >= 0; i-- {
				if req.Messages[i].Role != "assistant" {
					continue
				}
				if marker := FindLatestApprovalIDMarker(flattenAnthropicUserText(req.Messages[i].Content)); marker != "" {
					return verb, marker
				}
			}
		}
		return verb, id
	}
	// No plain-text approval reply on the user turn. Try the
	// AskUserQuestion path: the model surfaced the yes/no through the
	// harness's structured AskUserQuestion tool, and the user's choice
	// arrives as a tool_result block (not as text). Pair each
	// tool_result with the assistant's prior AskUserQuestion tool_use
	// and parse the chosen answer.
	return anthropicAskUserQuestionApprovalReply(req.Messages, userIdx)
}

// anthropicAskUserQuestionApprovalReply scans the latest user turn for
// a tool_result whose parent assistant tool_use is an AskUserQuestion
// call that carries the inline-approval ID marker in its question
// text. The tool_result content is the user's chosen answer (e.g.
// "yes" or "no"), which the existing verb parser normalizes to
// approve/deny.
//
// Returns ("", "") when no matching pair is found. Callers fall
// through to the legacy text path, so a harness that doesn't expose
// AskUserQuestion keeps the existing behavior unchanged.
func anthropicAskUserQuestionApprovalReply(
	messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	},
	userIdx int,
) (verb, id string) {
	results := extractAnthropicToolResults(messages[userIdx].Content)
	if len(results) == 0 {
		return "", ""
	}
	// Build a map of tool_use_id → question text for every
	// AskUserQuestion call in any prior assistant turn. We scan all
	// assistant turns (not just the most recent) because the harness
	// may send multiple tool_results in a single user turn after
	// several parallel AskUserQuestion calls.
	questions := collectAnthropicAskUserQuestionInputs(messages, userIdx)
	if len(questions) == 0 {
		return "", ""
	}
	// Walk tool_results in reverse so the latest answer wins when the
	// same hold has multiple tool_results in the same turn (unusual
	// but possible if the harness retries).
	for i := len(results) - 1; i >= 0; i-- {
		tr := results[i]
		questionText, ok := questions[tr.ToolUseID]
		if !ok {
			continue
		}
		marker := FindLatestApprovalIDMarker(questionText)
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
			// Some harnesses pass the answer through unwrapped; fall
			// back to the generic parser so a plain "yes" tool_result
			// content still releases the hold.
			answer = tr.Content
		}
		v, _ := ParseApprovalReplyText(answer)
		if v == "" {
			continue
		}
		return v, marker
	}
	return "", ""
}

// askUserQuestionAnswerRE pulls the answer label out of the harness-
// wrapped AskUserQuestion tool_result content. The leading question
// text is unbounded (spans newlines, contains arbitrary characters),
// the answer is what follows the trailing `"="` separator and lives
// between the next pair of double quotes. Non-greedy `[^"]*` on the
// answer keeps the match anchored to the rightmost answer slot when
// the wrapper trailer omits the period (older harness builds).
var askUserQuestionAnswerRE = regexp.MustCompile(`"="([^"]*)"`)

// ExtractAskUserQuestionAnswer is the exported entry point sibling
// packages (the llmproxy body editor) call so they share the same
// wrapper-unwrap logic as the release path.
func ExtractAskUserQuestionAnswer(content string) string {
	return extractAskUserQuestionAnswer(content)
}

// extractAskUserQuestionAnswer returns the answer label embedded in a
// harness-wrapped AskUserQuestion result. Returns "" when the
// content doesn't match the wrapper shape — callers fall through to
// the generic verb parser on the raw content.
func extractAskUserQuestionAnswer(content string) string {
	matches := askUserQuestionAnswerRE.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return ""
	}
	// Last match handles multi-question results (each question gets
	// its own `"="..."` slot). For an inline-approval prompt we only
	// emit a single question, so the last match equals the only
	// match in production; the slice walk just makes the contract
	// explicit for future multi-question reuse.
	return matches[len(matches)-1][1]
}

type anthropicToolResult struct {
	ToolUseID string
	Content   string
}

// extractAnthropicToolResults pulls tool_result blocks out of an
// Anthropic role:"user" content field, flattening any text-shaped
// content to a single string per block. Non-text content (image, etc.)
// is skipped — the AskUserQuestion answer is always text.
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

// flattenAnthropicToolResultContent accepts the two shapes Anthropic's
// tool_result.content field permits — a bare string or an array of
// typed blocks — and returns the concatenated text. JSON-typed answer
// payloads (e.g. {"answers":{"...":"yes"}}) flatten to their raw JSON
// here; ParseApprovalReplyText then picks the verb out of the embedded
// text. Empty/unparseable input returns "".
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

// collectAnthropicAskUserQuestionInputs walks every assistant turn
// before userIdx and returns a map of AskUserQuestion tool_use IDs to
// the marker-search text for that specific call: the AskUserQuestion
// input JSON PLUS the text blocks that PRECEDE it in the same
// assistant message (up to the previous AskUserQuestion, or the
// start of the message).
//
// Per-call pairing matters when an assistant turn contains multiple
// AskUserQuestion calls: the inline-approval renderer emits each
// call right after its own [clawvisor:approval=...] marker text, so
// the marker for call N MUST stay attached to call N and not bleed
// into calls N+1, N+2, etc. — otherwise a tool_result for one call
// could release the wrong hold.
func collectAnthropicAskUserQuestionInputs(
	messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	},
	userIdx int,
) map[string]string {
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
		// Single pass: accumulate text into pendingText. When we
		// hit an AskUserQuestion tool_use, flush pendingText into
		// its entry and reset — that text belonged to this call,
		// not any subsequent ones.
		var pendingTexts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				pendingTexts = append(pendingTexts, b.Text)
				continue
			}
			if b.Type != "tool_use" || b.Name != "AskUserQuestion" || b.ID == "" {
				continue
			}
			text := flattenAskUserQuestionInput(b.Input)
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

// flattenAskUserQuestionInput pulls human-readable question text out
// of an AskUserQuestion tool_use input. The schema nests questions
// under questions[].question plus questions[].options[].label /
// .description; we concatenate everything since the marker may live in
// the question or in an option description depending on how the model
// rendered it. The raw JSON is also included as a fallback so a
// model that drops the marker in an unexpected slot still matches.
func flattenAskUserQuestionInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

// SyntheticToolCall is one tool_use entry in a (possibly multi-block)
// synthetic approval-release response. The release path emits one of
// these per held tool_use when the user approves a coalesced hold.
type SyntheticToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

func SyntheticApprovalToolUseResponse(req *http.Request, provider Provider, requestBody []byte, allow bool, toolUseID, toolName string, toolInput map[string]any) (SyntheticApprovalResponse, bool) {
	return SyntheticApprovalToolUseResponseWithDenyMessage(req, provider, requestBody, allow, toolUseID, toolName, toolInput, ApprovalDeniedMessage)
}

func SyntheticApprovalToolUseResponseWithDenyMessage(req *http.Request, provider Provider, requestBody []byte, allow bool, toolUseID, toolName string, toolInput map[string]any, denyMessage string) (SyntheticApprovalResponse, bool) {
	calls := []SyntheticToolCall{{ID: toolUseID, Name: toolName, Input: toolInput}}
	return SyntheticApprovalToolUsesResponseWithDenyMessage(req, provider, requestBody, allow, calls, denyMessage)
}

// SyntheticApprovalToolUsesResponseWithDenyMessage builds a synthetic
// upstream response that carries N tool_use blocks on approve (or a
// single text block on deny). When len(calls) == 1 the wire shape is
// byte-identical to the single-call helper. When len(calls) > 1 the
// shape is a multi-block assistant turn (Anthropic content[], OpenAI
// Responses output[], OpenAI Chat tool_calls[]).
//
// Used by the coalesced-approval release path: one user yes/no covers
// every held tool_use in the turn, so the synthetic response must
// surface every approved call back to the harness for execution.
func SyntheticApprovalToolUsesResponseWithDenyMessage(req *http.Request, provider Provider, requestBody []byte, allow bool, calls []SyntheticToolCall, denyMessage string) (SyntheticApprovalResponse, bool) {
	denyMessage = strings.TrimSpace(denyMessage)
	if denyMessage == "" {
		denyMessage = ApprovalDeniedMessage
	}
	if allow && len(calls) == 0 {
		return SyntheticApprovalResponse{}, false
	}
	contentType := "application/json"
	var body []byte
	switch provider {
	case ProviderAnthropic:
		stream := AnthropicRequestWantsStream(requestBody)
		if allow {
			if stream {
				contentType = "text/event-stream"
				body = SynthAnthropicToolUsesSSE("", "", "assistant", calls)
			} else {
				body = SynthAnthropicToolUsesJSON("", "", "assistant", calls)
			}
		} else if stream {
			contentType = "text/event-stream"
			body = SynthAnthropicTextSSE("", "", "assistant", denyMessage)
		} else {
			body = SynthAnthropicTextJSON("", "", "assistant", denyMessage)
		}
	case ProviderOpenAI:
		stream := OpenAIRequestWantsStream(requestBody)
		if IsOpenAIChatCompletionsEndpoint(req) {
			if allow {
				if stream {
					contentType = "text/event-stream"
					body = SynthOpenAIChatToolCallsSSE(calls)
				} else {
					body = SynthOpenAIChatToolCallsJSON(calls)
				}
			} else if stream {
				contentType = "text/event-stream"
				body = SynthOpenAIChatTextSSE(denyMessage)
			} else {
				body = SynthOpenAIChatTextJSON(denyMessage)
			}
		} else if allow {
			if stream {
				contentType = "text/event-stream"
				body = SynthOpenAIResponsesFunctionCallsSSE(calls)
			} else {
				body = SynthOpenAIResponsesFunctionCallsJSON(calls)
			}
		} else if stream {
			contentType = "text/event-stream"
			body = SynthOpenAIResponsesTextSSE(denyMessage)
		} else {
			body = SynthOpenAIResponsesTextJSON(denyMessage)
		}
	default:
		return SyntheticApprovalResponse{}, false
	}
	return SyntheticApprovalResponse{ContentType: contentType, Body: body}, len(body) > 0
}

func flattenAnthropicUserText(raw json.RawMessage) string {
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
	var out []string
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			out = append(out, block.Text)
		}
	}
	return strings.Join(out, "\n")
}
