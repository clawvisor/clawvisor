package llmproxy

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
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
	switch req.Provider {
	case conversation.ProviderAnthropic:
		return stripAnthropicSyntheticApprovalHistory(req.Body)
	default:
		return SyntheticApprovalHistoryStripResult{Body: req.Body}, nil
	}
}

func stripAnthropicSyntheticApprovalHistory(body []byte) (SyntheticApprovalHistoryStripResult, error) {
	if !strings.Contains(string(body), "Clawvisor") {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}
	rawMessages, ok := raw["messages"]
	if !ok {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}
	out := make([]map[string]json.RawMessage, 0, len(messages))
	modified := false
	skipNextBareApprovalReply := false
	for _, msg := range messages {
		role := rawMessageString(msg["role"])
		contentText := flattenAnthropicTaskReplyText(msg["content"])
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
		out = append(out, msg)
	}
	if !modified || len(out) == 0 {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return SyntheticApprovalHistoryStripResult{Body: body}, err
	}
	raw["messages"] = encoded
	next, err := json.Marshal(raw)
	if err != nil {
		return SyntheticApprovalHistoryStripResult{Body: body}, err
	}
	return SyntheticApprovalHistoryStripResult{Body: next, Modified: true}, nil
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
	if containsInlineApprovalAugmentationMarker(text) ||
		strings.Contains(text, InlineTaskDenyMarker) ||
		strings.Contains(text, InlineTaskCreatorErrorMarker) {
		return false
	}
	verb, _ := conversation.ParseApprovalReplyText(text)
	return verb != ""
}
