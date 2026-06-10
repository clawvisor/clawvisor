package approvaltext

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestApprovalPromptIncludesCvReason(t *testing.T) {
	tu := conversation.ToolUse{
		Name:     "Bash",
		Input:    json.RawMessage(`{"command":"ls"}`),
		CvReason: "Inspecting working directory before edits.",
	}
	prompt := ApprovalPrompt(tu, "policy: shell command", "appr_1")
	if !strings.Contains(prompt, "Agent says: Inspecting working directory before edits.") {
		t.Fatalf("approval prompt missing cvreason; got:\n%s", prompt)
	}
}

func TestApprovalPromptOmitsCvReasonLineWhenEmpty(t *testing.T) {
	tu := conversation.ToolUse{
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"ls"}`),
	}
	prompt := ApprovalPrompt(tu, "policy: shell command", "appr_1")
	if strings.Contains(prompt, "Agent says") {
		t.Fatalf("approval prompt should omit cvreason line when empty; got:\n%s", prompt)
	}
}
