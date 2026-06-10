package llmproxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/jsonsurgery"
)

type approvalBodyEditor interface {
	LatestApprovalReply() (verb, approvalID string, ok bool)
	// ReplaceLatestUserText replaces the latest user-role message text
	// after confirming it parses as a reply with the expected verb. If
	// expectedApprovalID is non-empty, the message MUST also carry a
	// matching approval ID — without this check, a hold resolved by
	// Peek+ApprovalID could be released by a different verb-matching
	// message that races into the body between peek and rewrite.
	ReplaceLatestUserText(expectedVerb, expectedApprovalID, replacement string) ([]byte, bool, error)
	AugmentInlineApprovalHistory(outcomes InlineApprovalOutcomeStore, userID, agentID string) ([]byte, bool, error)
}

func newApprovalBodyEditor(req *http.Request, provider conversation.Provider, body []byte) (approvalBodyEditor, bool) {
	switch provider {
	case conversation.ProviderAnthropic:
		return anthropicApprovalBodyEditor{body: body}, true
	case conversation.ProviderOpenAI:
		if conversation.IsOpenAIChatCompletionsEndpoint(req) {
			return openAIChatApprovalBodyEditor{body: body}, true
		}
		return openAIResponsesApprovalBodyEditor{body: body}, true
	default:
		return nil, false
	}
}

type anthropicApprovalBodyEditor struct {
	body []byte
}

func (e anthropicApprovalBodyEditor) LatestApprovalReply() (string, string, bool) {
	verb, approvalID := conversation.AnthropicApprovalReply(e.body)
	return verb, approvalID, verb != ""
}

func (e anthropicApprovalBodyEditor) ReplaceLatestUserText(expectedVerb, expectedApprovalID, replacement string) ([]byte, bool, error) {
	return replaceAnthropicApprovalReply(e.body, expectedVerb, expectedApprovalID, replacement)
}

func (e anthropicApprovalBodyEditor) AugmentInlineApprovalHistory(outcomes InlineApprovalOutcomeStore, userID, agentID string) ([]byte, bool, error) {
	return augmentAnthropicApprovedInlineTasks(e.body, outcomes, userID, agentID)
}

type openAIChatApprovalBodyEditor struct {
	body []byte
}

func (e openAIChatApprovalBodyEditor) LatestApprovalReply() (string, string, bool) {
	verb, approvalID := conversation.OpenAIApprovalReply(e.body)
	return verb, approvalID, verb != ""
}

func (e openAIChatApprovalBodyEditor) ReplaceLatestUserText(expectedVerb, expectedApprovalID, replacement string) ([]byte, bool, error) {
	return replaceOpenAIChatApprovalReply(e.body, expectedVerb, expectedApprovalID, replacement)
}

func (e openAIChatApprovalBodyEditor) AugmentInlineApprovalHistory(_ InlineApprovalOutcomeStore, _, _ string) ([]byte, bool, error) {
	return e.body, false, nil
}

type openAIResponsesApprovalBodyEditor struct {
	body []byte
}

func (e openAIResponsesApprovalBodyEditor) LatestApprovalReply() (string, string, bool) {
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(e.body, &req); err == nil && len(req.Input) > 0 {
		var input string
		if err := json.Unmarshal(req.Input, &input); err == nil {
			verb, approvalID := conversation.ParseApprovalReplyText(input)
			return verb, approvalID, verb != ""
		}
	}
	verb, approvalID := conversation.OpenAIApprovalReply(e.body)
	return verb, approvalID, verb != ""
}

func (e openAIResponsesApprovalBodyEditor) ReplaceLatestUserText(expectedVerb, expectedApprovalID, replacement string) ([]byte, bool, error) {
	return replaceOpenAIResponsesApprovalReply(e.body, expectedVerb, expectedApprovalID, replacement)
}

func (e openAIResponsesApprovalBodyEditor) AugmentInlineApprovalHistory(_ InlineApprovalOutcomeStore, _, _ string) ([]byte, bool, error) {
	return e.body, false, nil
}

func replaceAnthropicApprovalReply(body []byte, expectedVerb, expectedApprovalID, replacement string) ([]byte, bool, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false, err
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		text := flattenAnthropicTaskReplyText(req.Messages[i].Content)
		verb, parsedID := conversation.ParseApprovalReplyText(text)
		if verb == "" {
			// No plain-text reply. Try the AskUserQuestion shape — the
			// user's answer arrives as a tool_result block whose parent
			// assistant tool_use is AskUserQuestion(question carrying
			// the inline-approval ID marker).
			rewritten, replaced, ok := replaceAnthropicAskUserQuestionApprovalReply(req.Messages, i, expectedVerb, expectedApprovalID, replacement)
			if !ok {
				return body, false, nil
			}
			req.Messages[i].Content = rewritten
			if !replaced {
				return body, false, nil
			}
			messages, err := json.Marshal(req.Messages)
			if err != nil {
				return nil, false, err
			}
			raw["messages"] = messages
			out, err := json.Marshal(raw)
			return out, err == nil, err
		}
		if verb != expectedVerb {
			return body, false, nil
		}
		if !approvalIDMatchesExpectation(parsedID, expectedApprovalID) {
			return body, false, nil
		}
		encoded, _ := json.Marshal(replacement)
		req.Messages[i].Content = encoded
		messages, err := json.Marshal(req.Messages)
		if err != nil {
			return nil, false, err
		}
		raw["messages"] = messages
		out, err := json.Marshal(raw)
		return out, err == nil, err
	}
	return body, false, nil
}

// replaceAnthropicAskUserQuestionApprovalReply rewrites the
// tool_result content of the user's matching AskUserQuestion answer
// to the replacement notice. Returns (newContent, replaced, ok):
//
//   - ok=false: the user message isn't an AskUserQuestion-shaped
//     approval reply at all (defer to text-path return false).
//   - ok=true, replaced=false: AskUserQuestion shape matched but the
//     verb or approvalID didn't (caller should return false to abort
//     the rewrite, mirroring the text path's mismatch return).
//   - ok=true, replaced=true: tool_result rewritten in place.
//
// The tool_use_id is preserved so the harness still associates the
// reshaped result with the original AskUserQuestion call. Replacing
// just the content (not the whole block) keeps the model from seeing
// an orphaned tool_use.
func replaceAnthropicAskUserQuestionApprovalReply(
	messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	},
	userIdx int,
	expectedVerb, expectedApprovalID, replacement string,
) (json.RawMessage, bool, bool) {
	results := extractAnthropicToolResultBlocks(messages[userIdx].Content)
	if len(results) == 0 {
		return nil, false, false
	}
	questions := collectAnthropicAskUserQuestionInputs(messages, userIdx)
	if len(questions) == 0 {
		return nil, false, false
	}
	// Find the latest tool_result whose parent tool_use is an
	// AskUserQuestion call with a marker. Matches the detection order
	// in anthropicAskUserQuestionApprovalReply so probe and rewrite
	// converge on the same block.
	targetIdx := -1
	var verb, parsedID string
	for i := len(results) - 1; i >= 0; i-- {
		tr := results[i]
		questionText, ok := questions[tr.toolUseID]
		if !ok {
			continue
		}
		marker := conversation.FindLatestApprovalIDMarker(questionText)
		if marker == "" {
			continue
		}
		// Same wrapper-unwrap as approval_release.go's detection.
		// Claude Code emits the harness-wrapped form; older / other
		// harnesses may pass the answer through unwrapped, so fall
		// back to the raw content when the wrapper isn't matched.
		answer := conversation.ExtractAskUserQuestionAnswer(tr.content)
		if answer == "" {
			answer = tr.content
		}
		v, _ := conversation.ParseApprovalReplyText(answer)
		if v == "" {
			continue
		}
		targetIdx = i
		verb = v
		parsedID = marker
		break
	}
	if targetIdx < 0 {
		return nil, false, false
	}
	if verb != expectedVerb {
		return messages[userIdx].Content, true, false
	}
	if !approvalIDMatchesExpectation(parsedID, expectedApprovalID) {
		return messages[userIdx].Content, true, false
	}
	// Rebuild the content array with the chosen tool_result's content
	// replaced by the notice. Other blocks pass through unchanged.
	newContent, ok := rewriteAnthropicToolResultContent(messages[userIdx].Content, results[targetIdx].toolUseID, replacement)
	if !ok {
		return messages[userIdx].Content, true, false
	}
	return newContent, true, true
}

// collectAnthropicAskUserQuestionInputs walks every assistant message
// before userIdx and returns a map of AskUserQuestion tool_use IDs to
// the marker-search text for that specific call: the AskUserQuestion
// input JSON plus the text blocks PRECEDING it in the same assistant
// message (up to the prior AskUserQuestion). Mirrors the
// conversation-package helper of the same name.
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

type anthropicToolResultBlock struct {
	toolUseID string
	content   string
}

func extractAnthropicToolResultBlocks(raw json.RawMessage) []anthropicToolResultBlock {
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
	var out []anthropicToolResultBlock
	for _, b := range blocks {
		if b.Type != "tool_result" || b.ToolUseID == "" {
			continue
		}
		content := flattenAskUserQuestionToolResult(b.Content)
		if content == "" {
			continue
		}
		out = append(out, anthropicToolResultBlock{toolUseID: b.ToolUseID, content: content})
	}
	return out
}

func flattenAskUserQuestionToolResult(raw json.RawMessage) string {
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
	// JSON-object answer payload (e.g. {"answers":{"...":"yes"}}):
	// fall back to the raw bytes so the verb parser can pick a
	// "yes"/"no" token out of the embedded value.
	return string(raw)
}

// rewriteAnthropicToolResultContent REPLACES the tool_result block
// whose tool_use_id matches targetToolUseID with a plain text block
// carrying the replacement string. This is the AskUserQuestion
// counterpart to the text-substitution path's "replace the user's
// 'yes' content with the notice" — done as a block-shape swap so:
//
//  1. The orphan tool_use_id (whose parent AskUserQuestion call gets
//     stripped from history alongside the approval prompt) no longer
//     refers to anything — Anthropic stops 400'ing the next request.
//  2. The notice text survives the historystrip's bare-verb check
//     (the notice carries the <clawvisor-notice kind="task-...">
//     marker, so isBareSyntheticApprovalReply returns false).
//
// Other blocks (text, image, additional tool_results) pass through
// unchanged.
func rewriteAnthropicToolResultContent(raw json.RawMessage, targetToolUseID, replacement string) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false
	}
	rewritten := false
	for i, blk := range blocks {
		var probe struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
		}
		if err := json.Unmarshal(blk, &probe); err != nil {
			continue
		}
		if probe.Type != "tool_result" || probe.ToolUseID != targetToolUseID {
			continue
		}
		// Swap the tool_result block for a text block carrying the
		// notice. See the rewriteAnthropicToolResultContent header
		// for why we replace the block shape rather than just the
		// content field.
		textBlock := map[string]string{"type": "text", "text": replacement}
		newBlock, err := json.Marshal(textBlock)
		if err != nil {
			continue
		}
		blocks[i] = newBlock
		rewritten = true
	}
	if !rewritten {
		return nil, false
	}
	out, err := json.Marshal(blocks)
	if err != nil {
		return nil, false
	}
	return out, true
}

// approvalIDMatchesExpectation enforces the parsed approval ID against
// the caller's expectation ONLY when the user actually typed an ID.
// The documented common case is a bare verb like "approve" / "yes" /
// "deny" / "no" with no ID — for those, fall through to verb-only
// matching (existing behavior).
//
// The stricter rule fires for explicit-ID replies ("approve cv-…"):
// when the parsed ID is present but doesn't match the hold Peek
// resolved, refuse to rewrite so the wrong hold can't be released by
// a verb-matching message that races into the body between peek and
// rewrite. A model that copies the ID-stamped prompt back into a
// later turn — or a malicious / confused agent that swaps IDs in a
// chained release — falls into this stricter path.
func approvalIDMatchesExpectation(parsed, expected string) bool {
	if expected == "" || parsed == "" {
		return true
	}
	return parsed == expected
}

func replaceOpenAIChatApprovalReply(body []byte, expectedVerb, expectedApprovalID, replacement string) ([]byte, bool, error) {
	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false, err
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		role, _ := req.Messages[i]["role"].(string)
		if role != "user" {
			continue
		}
		contentRaw, _ := json.Marshal(req.Messages[i]["content"])
		verb, parsedID := conversation.ParseApprovalReplyText(flattenOpenAITaskReplyContent(contentRaw))
		if verb != expectedVerb {
			return body, false, nil
		}
		if !approvalIDMatchesExpectation(parsedID, expectedApprovalID) {
			return body, false, nil
		}
		req.Messages[i]["content"] = replacement
		messages, err := json.Marshal(req.Messages)
		if err != nil {
			return nil, false, err
		}
		raw["messages"] = messages
		out, err := json.Marshal(raw)
		return out, err == nil, err
	}
	return body, false, nil
}

func replaceOpenAIResponsesApprovalReply(body []byte, expectedVerb, expectedApprovalID, replacement string) ([]byte, bool, error) {
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Input) == 0 {
		return body, false, err
	}
	var inputString string
	if err := json.Unmarshal(req.Input, &inputString); err == nil {
		verb, parsedID := conversation.ParseApprovalReplyText(inputString)
		if verb != expectedVerb {
			return body, false, nil
		}
		if !approvalIDMatchesExpectation(parsedID, expectedApprovalID) {
			return body, false, nil
		}
		encoded, _ := json.Marshal(replacement)
		raw["input"] = encoded
		out, err := json.Marshal(raw)
		return out, err == nil, err
	}
	var items []map[string]any
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return body, false, nil
	}
	for i := len(items) - 1; i >= 0; i-- {
		typ, _ := items[i]["type"].(string)
		role, _ := items[i]["role"].(string)
		if typ != "message" || role != "user" {
			continue
		}
		contentRaw, _ := json.Marshal(items[i]["content"])
		verb, parsedID := conversation.ParseApprovalReplyText(flattenOpenAITaskReplyContent(contentRaw))
		if verb != expectedVerb {
			return body, false, nil
		}
		if !approvalIDMatchesExpectation(parsedID, expectedApprovalID) {
			return body, false, nil
		}
		items[i]["content"] = []map[string]any{{"type": "input_text", "text": replacement}}
		input, err := json.Marshal(items)
		if err != nil {
			return nil, false, err
		}
		raw["input"] = input
		out, err := json.Marshal(raw)
		return out, err == nil, err
	}
	return body, false, nil
}

func augmentAnthropicApprovedInlineTasks(body []byte, outcomes InlineApprovalOutcomeStore, userID, agentID string) ([]byte, bool, error) {
	// Byte fidelity invariant: unchanged messages pass through verbatim.
	// Only the user message whose content we're augmenting is reshaped.
	// Top-level body keys keep their incoming order. See SanitizeAnthropicRequest
	// for why this matters.
	msgsStart, msgsEnd, ok := jsonsurgery.FindFieldValue(body, "messages")
	if !ok {
		return body, false, nil
	}
	messages, ok := jsonsurgery.FlattenArray(body[msgsStart:msgsEnd])
	if !ok {
		return body, false, nil
	}
	newMessages := make([]json.RawMessage, len(messages))
	copy(newMessages, messages)
	changed := false

	for i := 1; i < len(messages); i++ {
		msg := messages[i]
		roleStart, roleEnd, ok := jsonsurgery.FindFieldValue(msg, "role")
		if !ok {
			continue
		}
		var role string
		if err := json.Unmarshal(msg[roleStart:roleEnd], &role); err != nil || role != "user" {
			continue
		}
		contentStart, contentEnd, ok := jsonsurgery.FindFieldValue(msg, "content")
		if !ok {
			continue
		}
		content := msg[contentStart:contentEnd]
		userText := flattenAnthropicTaskReplyText(content)
		verb, _ := conversation.ParseApprovalReplyText(userText)
		if verb != "approve" {
			continue
		}
		if containsInlineApprovalAugmentationMarker(userText) {
			continue
		}

		priorMsg := messages[i-1]
		priorRoleStart, priorRoleEnd, ok := jsonsurgery.FindFieldValue(priorMsg, "role")
		if !ok {
			continue
		}
		var priorRole string
		if err := json.Unmarshal(priorMsg[priorRoleStart:priorRoleEnd], &priorRole); err != nil || priorRole != "assistant" {
			continue
		}
		priorContentStart, priorContentEnd, ok := jsonsurgery.FindFieldValue(priorMsg, "content")
		if !ok {
			continue
		}
		priorText := flattenAnthropicTaskReplyText(priorMsg[priorContentStart:priorContentEnd])
		if !strings.Contains(priorText, InlineApprovalSubstitutedPromptMarker) {
			continue
		}

		approvalID := extractApprovalIDFromPrompt(priorText)
		note, ok := augmentationContextForOutcome(InlineApprovalOutcomeKey{
			UserID:     userID,
			AgentID:    agentID,
			ApprovalID: approvalID,
		}, outcomes)
		if !ok {
			continue
		}

		updatedContent, ok := augmentUserContent(content, verb, note)
		if !ok {
			continue
		}
		newMsg, err := jsonsurgery.SetField(msg, "content", updatedContent)
		if err != nil {
			continue
		}
		newMessages[i] = newMsg
		changed = true
	}

	if !changed {
		return body, false, nil
	}
	newMsgsBytes, err := json.Marshal(newMessages)
	if err != nil {
		return body, false, err
	}
	out, err := jsonsurgery.SetField(body, "messages", newMsgsBytes)
	if err != nil {
		return body, false, err
	}
	return out, true, nil
}

func augmentUserContent(content json.RawMessage, _ string, note string) (json.RawMessage, bool) {
	if len(content) == 0 {
		encoded, err := json.Marshal(note)
		return encoded, err == nil
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		encoded, marshalErr := json.Marshal(note)
		return encoded, marshalErr == nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, false
	}
	spliceAt := -1
	for i, blk := range blocks {
		var t string
		if err := json.Unmarshal(blk["type"], &t); err != nil {
			continue
		}
		if t != "text" {
			continue
		}
		var text string
		if err := json.Unmarshal(blk["text"], &text); err != nil {
			continue
		}
		if v, _ := conversation.ParseApprovalReplyText(text); v == "" {
			continue
		}
		if spliceAt < 0 {
			spliceAt = i
		}
		stripped := stripBareApprovalLines(text)
		encoded, err := json.Marshal(stripped)
		if err != nil {
			return nil, false
		}
		blocks[i]["text"] = encoded
	}
	if spliceAt < 0 {
		return nil, false
	}
	var spliceText string
	_ = json.Unmarshal(blocks[spliceAt]["text"], &spliceText)
	newSpliceText := note
	if spliceText != "" {
		newSpliceText = spliceText + "\n\n" + note
	}
	encoded, err := json.Marshal(newSpliceText)
	if err != nil {
		return nil, false
	}
	blocks[spliceAt]["text"] = encoded

	kept := blocks[:0]
	for _, blk := range blocks {
		var t string
		if err := json.Unmarshal(blk["type"], &t); err == nil && t == "text" {
			var bt string
			if err := json.Unmarshal(blk["text"], &bt); err == nil && bt == "" {
				continue
			}
		}
		kept = append(kept, blk)
	}

	out, err := json.Marshal(kept)
	if err != nil {
		return nil, false
	}
	return out, true
}

func stripBareApprovalLines(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		probe := strings.TrimSpace(line)
		if probe == "" {
			kept = append(kept, line)
			continue
		}
		if verb, _ := conversation.ParseApprovalReplyText(probe); verb != "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func flattenAnthropicTaskReplyText(raw json.RawMessage) string {
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
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func flattenOpenAITaskReplyContent(raw json.RawMessage) string {
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
		switch b.Type {
		case "text", "input_text", "output_text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}
