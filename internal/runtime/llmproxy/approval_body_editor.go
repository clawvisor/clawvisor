package llmproxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
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
		verb, parsedID := conversation.ParseApprovalReplyText(flattenAnthropicTaskReplyText(req.Messages[i].Content))
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
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, false, err
	}
	rawMessages, ok := raw["messages"]
	if !ok {
		return body, false, nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return body, false, err
	}

	changed := false

	for i := 1; i < len(messages); i++ {
		var role string
		if err := json.Unmarshal(messages[i]["role"], &role); err != nil || role != "user" {
			continue
		}
		userText := flattenAnthropicTaskReplyText(messages[i]["content"])
		verb, _ := conversation.ParseApprovalReplyText(userText)
		if verb != "approve" {
			continue
		}
		if containsInlineApprovalAugmentationMarker(userText) {
			continue
		}

		var priorRole string
		if err := json.Unmarshal(messages[i-1]["role"], &priorRole); err != nil || priorRole != "assistant" {
			continue
		}
		priorText := flattenAnthropicTaskReplyText(messages[i-1]["content"])
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

		updated, ok := augmentUserContent(messages[i]["content"], verb, note)
		if !ok {
			continue
		}
		messages[i]["content"] = updated
		changed = true
	}

	if !changed {
		return body, false, nil
	}
	updatedMessages, err := json.Marshal(messages)
	if err != nil {
		return body, false, err
	}
	raw["messages"] = updatedMessages
	out, err := json.Marshal(raw)
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
