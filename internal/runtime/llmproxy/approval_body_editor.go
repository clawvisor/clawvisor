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
	//
	// reconstruction, when non-nil, signals the editor to ALSO
	// reconstruct the model's original tool_use that the proxy
	// substituted away. For Anthropic + AskUserQuestion shape this
	// means: replace the substituted-prompt assistant turn with a
	// synthetic [tool_use(original)] turn, and pair a synthetic
	// tool_result block alongside the replacement text in the user
	// turn. Other shapes (plain-text approval, openAI providers) fall
	// back to the no-reconstruction path silently. The body editor
	// owns codec-specific shape decisions; the caller just hands over
	// the snapshot.
	ReplaceLatestUserText(expectedVerb, expectedApprovalID, replacement string, reconstruction *InlineApprovalOriginalCall) ([]byte, bool, error)
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
	// conversation.AnthropicApprovalReply tries the text path first
	// and falls back to the AskUserQuestion shape internally, so
	// every caller of the shared entry point (lite-proxy body
	// editor, runtime proxy, ad-hoc tooling) sees the same release
	// behavior.
	verb, approvalID := conversation.AnthropicApprovalReply(e.body)
	return verb, approvalID, verb != ""
}

func (e anthropicApprovalBodyEditor) ReplaceLatestUserText(expectedVerb, expectedApprovalID, replacement string, reconstruction *InlineApprovalOriginalCall) ([]byte, bool, error) {
	return replaceAnthropicApprovalReply(e.body, expectedVerb, expectedApprovalID, replacement, reconstruction)
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

func (e openAIChatApprovalBodyEditor) ReplaceLatestUserText(expectedVerb, expectedApprovalID, replacement string, _ *InlineApprovalOriginalCall) ([]byte, bool, error) {
	// OpenAI Chat Completions has no AskUserQuestion-equivalent
	// substituted-tool-use shape, so reconstruction is a no-op here.
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

func (e openAIResponsesApprovalBodyEditor) ReplaceLatestUserText(expectedVerb, expectedApprovalID, replacement string, _ *InlineApprovalOriginalCall) ([]byte, bool, error) {
	// OpenAI Responses has no AskUserQuestion-equivalent
	// substituted-tool-use shape (the substitution lands as plain
	// text in this codec), so reconstruction is a no-op here.
	return replaceOpenAIResponsesApprovalReply(e.body, expectedVerb, expectedApprovalID, replacement)
}

func (e openAIResponsesApprovalBodyEditor) AugmentInlineApprovalHistory(_ InlineApprovalOutcomeStore, _, _ string) ([]byte, bool, error) {
	return e.body, false, nil
}

func replaceAnthropicApprovalReply(body []byte, expectedVerb, expectedApprovalID, replacement string, reconstruction *InlineApprovalOriginalCall) ([]byte, bool, error) {
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
			// No plain-text reply. Try the AskUserQuestion shape —
			// the user's answer arrives as a tool_result block whose
			// parent assistant tool_use is AskUserQuestion with the
			// inline-approval ID marker. The fallback parses the
			// body itself, so we return its result directly here.
			return rewriteAnthropicAskUserQuestionApprovalReply(body, expectedVerb, expectedApprovalID, replacement, reconstruction)
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

// rewriteAnthropicAskUserQuestionApprovalReply is the body-editor
// fallback for the AskUserQuestion shape: when the latest user turn
// has no plain-text approval verb, the user's choice may instead
// have arrived as a tool_result block linked to a synthesized
// AskUserQuestion picker. This finds the answered call, validates
// it against the expected verb/approvalID, and REPLACES the
// tool_result block with a text block carrying the replacement
// notice — block-shape swap so:
//
//  1. The orphan tool_use_id (whose parent AskUserQuestion call
//     gets stripped from history alongside the approval prompt) no
//     longer refers to anything — Anthropic stops 400'ing the next
//     request on orphan tool_result blocks.
//  2. The notice text survives the historystrip's bare-verb check
//     (the notice carries the <clawvisor-notice kind="task-...">
//     marker, so isBareSyntheticApprovalReply returns false).
//
// When reconstruction is non-nil, the rewriter ALSO replaces the
// preceding assistant turn (the substituted-prompt + AskUserQuestion
// pair) with a synthetic [tool_use(reconstruction)] turn, and pairs
// the user-turn replacement as a tool_result against that
// reconstructed tool_use_id. This restores the model's evidence of
// having called the substituted endpoint — without it the model has
// no record of having emitted the original POST and re-emits it on
// the next turn (the "agent keeps trying to expand" failure mode).
//
// Returns (body, false, nil) when the AskUserQuestion shape doesn't
// match or the verb/approvalID expectation fails. Other blocks
// (text, image, additional tool_results) pass through unchanged.
func rewriteAnthropicAskUserQuestionApprovalReply(body []byte, expectedVerb, expectedApprovalID, replacement string, reconstruction *InlineApprovalOriginalCall) ([]byte, bool, error) {
	match, ok := conversation.FindAnthropicAskUserQuestionApprovalMatch(body)
	if !ok {
		return body, false, nil
	}
	if match.Verb != expectedVerb {
		return body, false, nil
	}
	if !approvalIDMatchesExpectation(match.ApprovalID, expectedApprovalID) {
		return body, false, nil
	}
	// Re-parse just enough of the body to splice in the rewritten
	// content. The detector runs against an immutable snapshot;
	// the rewriter walks the same body to keep top-level keys in
	// the original order (byte-fidelity invariant the
	// historystrip's surveyors lean on).
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
	if match.UserIdx < 0 || match.UserIdx >= len(req.Messages) {
		return body, false, nil
	}

	// Reconstruction path: replace the substituted-prompt assistant
	// turn (synthetic AskUserQuestion + text) with a synthetic
	// [tool_use(original)] turn, AND swap the AskUserQuestion
	// tool_result for a tool_result PAIRED to the reconstructed
	// tool_use_id. The model sees its own call + a result, instead
	// of the absence of a call followed by a "you did this" notice.
	if reconstruction != nil && reconstruction.ToolUseID != "" {
		assistantIdx := match.UserIdx - 1
		if assistantIdx >= 0 && req.Messages[assistantIdx].Role == "assistant" {
			newAssistant, assistantOK := buildReconstructedAssistantContent(reconstruction)
			newUser, userOK := swapAnthropicToolResultForReconstructedPair(
				req.Messages[match.UserIdx].Content, match.ToolUseID, reconstruction.ToolUseID, replacement)
			if assistantOK && userOK {
				req.Messages[assistantIdx].Content = newAssistant
				req.Messages[match.UserIdx].Content = newUser
				messages, err := json.Marshal(req.Messages)
				if err != nil {
					return nil, false, err
				}
				raw["messages"] = messages
				out, err := json.Marshal(raw)
				return out, err == nil, err
			}
		}
		// Reconstruction shape didn't fit (no preceding assistant
		// turn, or block build failed). Fall through to the legacy
		// text-block swap so the user still sees the notice — the
		// model will re-emit but at least the conversation doesn't
		// stall.
	}

	newContent, swapped := swapAnthropicToolResultForTextBlock(req.Messages[match.UserIdx].Content, match.ToolUseID, replacement)
	if !swapped {
		return body, false, nil
	}
	req.Messages[match.UserIdx].Content = newContent
	messages, err := json.Marshal(req.Messages)
	if err != nil {
		return nil, false, err
	}
	raw["messages"] = messages
	out, err := json.Marshal(raw)
	return out, err == nil, err
}

// buildReconstructedAssistantContent renders the synthetic
// [tool_use] assistant turn carrying the model's original
// tool_use_id, name, and input. The input is taken verbatim from the
// hold record so the model sees the exact bytes it would have
// emitted. Returns (nil, false) when the original input is empty (no
// reconstruction possible).
func buildReconstructedAssistantContent(original *InlineApprovalOriginalCall) (json.RawMessage, bool) {
	if original == nil || original.ToolUseID == "" || original.ToolName == "" {
		return nil, false
	}
	input := original.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	block := map[string]any{
		"type":  "tool_use",
		"id":    original.ToolUseID,
		"name":  original.ToolName,
		"input": input,
	}
	raw, err := json.Marshal([]any{block})
	if err != nil {
		return nil, false
	}
	return raw, true
}

// swapAnthropicToolResultForReconstructedPair walks the user turn's
// content blocks and replaces the AskUserQuestion tool_result
// (whose tool_use_id matches askToolUseID) with a tool_result
// PAIRED to the reconstructed tool_use_id, carrying the replacement
// notice as its content. Other blocks survive unchanged. The pairing
// keeps the assistant→user tool_use/tool_result invariant Anthropic
// validates at request time.
func swapAnthropicToolResultForReconstructedPair(raw json.RawMessage, askToolUseID, reconstructedToolUseID, replacement string) (json.RawMessage, bool) {
	if len(raw) == 0 || reconstructedToolUseID == "" {
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
		if probe.Type != "tool_result" || probe.ToolUseID != askToolUseID {
			continue
		}
		newBlock, err := json.Marshal(map[string]any{
			"type":        "tool_result",
			"tool_use_id": reconstructedToolUseID,
			"content":     replacement,
		})
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

// swapAnthropicToolResultForTextBlock walks `raw` (a user message's
// content blocks array) and replaces the tool_result block whose
// tool_use_id matches targetToolUseID with a plain text block
// carrying replacement. Other blocks pass through unchanged. The
// block-shape swap (rather than a content-field rewrite) is what
// keeps the next request's history valid after historystrip drops
// the parent AskUserQuestion call — see
// rewriteAnthropicAskUserQuestionApprovalReply for the rationale.
func swapAnthropicToolResultForTextBlock(raw json.RawMessage, targetToolUseID, replacement string) (json.RawMessage, bool) {
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
		if !strings.Contains(priorText, InlineApprovalSubstitutedPromptMarker) &&
			!strings.Contains(priorText, InlineExpansionApprovalSubstitutedPromptMarker) {
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
