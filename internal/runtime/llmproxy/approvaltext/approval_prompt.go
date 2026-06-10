package approvaltext

import (
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

const InlineApprovalIDMarker = conversation.ApprovalIDMarker

func ApprovalIDFooter(approvalID string) string {
	if strings.TrimSpace(approvalID) == "" {
		return ""
	}
	return "\n\n" + InlineApprovalIDMarker + approvalID + "]"
}

func ApprovalPrompt(tu conversation.ToolUse, reason, approvalID string) string {
	preview := conversation.MakeToolInputPreview(tu.Input)
	var b strings.Builder
	b.WriteString("Clawvisor paused this tool call for approval.")
	if tu.Name != "" {
		b.WriteString("\n\nTool: `")
		b.WriteString(tu.Name)
		b.WriteString("`")
	}
	if reason != "" {
		b.WriteString("\nReason: ")
		b.WriteString(reason)
	}
	if cv := strings.TrimSpace(tu.CvReason); cv != "" {
		b.WriteString("\nAgent says: ")
		b.WriteString(cv)
	}
	if preview != "" {
		b.WriteString("\nInput: ")
		b.WriteString(preview)
	}
	b.WriteString("\n\nReply `yes` or `y` to run this tool call, `no` or `n` to block it, or `task` to instruct the agent to include this in a task definition for approval.")
	b.WriteString(ApprovalIDFooter(approvalID))
	return b.String()
}
