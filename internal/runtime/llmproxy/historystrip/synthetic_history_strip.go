package historystrip

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/jsonsurgery"
)

type SyntheticApprovalHistoryStripRequest struct {
	Provider conversation.Provider
	Body     []byte
}

type SyntheticApprovalHistoryStripResult struct {
	Body     []byte
	Modified bool
}

const ToolApprovalSubstitutedPromptMarker = "Clawvisor paused this tool call for approval."

// StripSyntheticApprovalHistory removes Clawvisor-generated approval UI from
// conversation history before it is sent back to the upstream model. The live
// pending-approval cache is the source of truth; historical assistant text that
// looks like an approval prompt is untrusted model context and can be copied or
// hallucinated by the model on later turns.
func StripSyntheticApprovalHistory(req SyntheticApprovalHistoryStripRequest) (SyntheticApprovalHistoryStripResult, error) {
	if len(req.Body) == 0 {
		return SyntheticApprovalHistoryStripResult{Body: req.Body}, nil
	}
	body := req.Body
	modified := false
	if req.Provider == conversation.ProviderAnthropic {
		res, err := stripAnthropicSyntheticApprovalHistory(body)
		if err != nil {
			return SyntheticApprovalHistoryStripResult{Body: body}, err
		}
		if res.Modified {
			body = res.Body
			modified = true
		}
		return SyntheticApprovalHistoryStripResult{Body: body, Modified: modified}, nil
	}
	return SyntheticApprovalHistoryStripResult{Body: body}, nil
}

func stripAnthropicSyntheticApprovalHistory(body []byte) (SyntheticApprovalHistoryStripResult, error) {
	if !strings.Contains(string(body), "Clawvisor") {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}
	// Byte fidelity invariant: surviving messages pass through verbatim.
	// Top-level body keys keep their order. Only messages we merge get
	// re-marshalled — and those are user messages, never assistants
	// carrying thinking blocks, so signature verification is unaffected.
	msgsStart, msgsEnd, ok := jsonsurgery.FindFieldValue(body, "messages")
	if !ok {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}
	messages, ok := jsonsurgery.FlattenArray(body[msgsStart:msgsEnd])
	if !ok {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}
	survivors := make([]json.RawMessage, 0, len(messages))
	modified := false
	skipNextBareApprovalReply := false
	for _, msg := range messages {
		role := extractMessageRole(msg)
		contentText := flattenAnthropicTaskReplyText(extractMessageContent(msg))
		if skipNextBareApprovalReply {
			skipNextBareApprovalReply = false
			if role == "user" && isBareSyntheticApprovalReply(contentText) {
				modified = true
				continue
			}
		}
		if role == "assistant" && isSyntheticApprovalPromptText(contentText) {
			modified = true
			skipNextBareApprovalReply = true
			continue
		}
		survivors = append(survivors, msg)
	}
	if !modified || len(survivors) == 0 {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}

	// Merge consecutive user messages that became adjacent after the strip.
	var merged []json.RawMessage
	for _, msg := range survivors {
		if len(merged) == 0 {
			merged = append(merged, msg)
			continue
		}
		prev := merged[len(merged)-1]
		prevRole := extractMessageRole(prev)
		currRole := extractMessageRole(msg)
		if prevRole == currRole && currRole == "user" {
			prevContent := extractMessageContent(prev)
			currContent := extractMessageContent(msg)
			if canMergeAnthropicContent(prevContent, currContent) {
				mergedContent, err := mergeAnthropicContent(prevContent, currContent)
				if err != nil {
					return SyntheticApprovalHistoryStripResult{Body: body}, err
				}
				newPrev, err := jsonsurgery.SetField(prev, "content", mergedContent)
				if err != nil {
					return SyntheticApprovalHistoryStripResult{Body: body}, err
				}
				merged[len(merged)-1] = newPrev
				continue
			}
		}
		merged = append(merged, msg)
	}

	newMsgsBytes, err := json.Marshal(merged)
	if err != nil {
		return SyntheticApprovalHistoryStripResult{Body: body}, err
	}
	next, err := jsonsurgery.SetField(body, "messages", newMsgsBytes)
	if err != nil {
		return SyntheticApprovalHistoryStripResult{Body: body}, err
	}
	return SyntheticApprovalHistoryStripResult{Body: next, Modified: true}, nil
}

func extractMessageRole(msg json.RawMessage) string {
	start, end, ok := jsonsurgery.FindFieldValue(msg, "role")
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(msg[start:end], &s)
	return s
}

func extractMessageContent(msg json.RawMessage) json.RawMessage {
	start, end, ok := jsonsurgery.FindFieldValue(msg, "content")
	if !ok {
		return nil
	}
	return msg[start:end]
}

func canMergeAnthropicContent(c1, c2 json.RawMessage) bool {
	var s1, s2 string
	err1 := json.Unmarshal(c1, &s1)
	err2 := json.Unmarshal(c2, &s2)
	if err1 == nil || err2 == nil {
		return err1 == nil && err2 == nil
	}
	var blocks1, blocks2 []json.RawMessage
	return json.Unmarshal(c1, &blocks1) == nil && json.Unmarshal(c2, &blocks2) == nil
}

func mergeAnthropicContent(c1, c2 json.RawMessage) (json.RawMessage, error) {
	if len(c1) == 0 {
		return c2, nil
	}
	if len(c2) == 0 {
		return c1, nil
	}

	var s1, s2 string
	err1 := json.Unmarshal(c1, &s1)
	err2 := json.Unmarshal(c2, &s2)

	if err1 == nil && err2 == nil {
		merged := s1 + "\n\n" + s2
		return json.Marshal(merged)
	}
	if err1 != nil && err2 == nil {
		var blocks1 []json.RawMessage
		if err := json.Unmarshal(c1, &blocks1); err != nil {
			return nil, err
		}
		blocks1 = append(blocks1, anthropicTextBlockRaw(s2))
		return json.Marshal(blocks1)
	}
	if err1 == nil && err2 != nil {
		var blocks2 []json.RawMessage
		if err := json.Unmarshal(c2, &blocks2); err != nil {
			return nil, err
		}
		// Use append-from-literal to sidestep CodeQL's
		// allocation-size-overflow warning on `len(blocks2)+1`.
		out := append([]json.RawMessage{anthropicTextBlockRaw(s1)}, blocks2...)
		return json.Marshal(out)
	}

	var blocks1 []json.RawMessage
	if err := json.Unmarshal(c1, &blocks1); err != nil {
		return nil, err
	}

	var blocks2 []json.RawMessage
	if err := json.Unmarshal(c2, &blocks2); err != nil {
		return nil, err
	}

	mergedBlocks := append(blocks1, blocks2...)
	return json.Marshal(mergedBlocks)
}

func anthropicTextBlockRaw(text string) json.RawMessage {
	block, _ := json.Marshal(map[string]string{
		"type": "text",
		"text": text,
	})
	return json.RawMessage(block)
}

func rawMessageString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func isSyntheticApprovalPromptText(text string) bool {
	return strings.Contains(text, InlineApprovalSubstitutedPromptMarker) ||
		strings.Contains(text, ToolApprovalSubstitutedPromptMarker)
}

func isBareSyntheticApprovalReply(text string) bool {
	// containsInlineApprovalAugmentationMarker recognizes every
	// proxy-substituted inline-task notice (approved / denied / error)
	// via the shared `<clawvisor-notice kind="task-` substring. A turn
	// carrying that substring is the proxy's own rewrite, not a bare
	// approval verb from the user.
	if containsInlineApprovalAugmentationMarker(text) {
		return false
	}
	verb, _ := conversation.ParseApprovalReplyText(text)
	return verb != ""
}
