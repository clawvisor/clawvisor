package llmproxy

import (
	"strings"
)

// renderScopeDriftOneOffPrompt builds the inline yes/no prompt the user
// sees when the agent emits a <clawvisor:decision option="one-off">
// markup against a blocked tool call. The output replaces the markup
// in the assistant text the model would otherwise have produced, so
// the user sees the proxy's question inline instead of the raw
// agent decision.
//
// The format mirrors renderTaskApprovalPrompt's shape so the user
// experiences both flows the same way:
//   - a one-line header naming the proxy's action
//   - the service.action being requested
//   - the agent's note (the markup body) on its own line
//   - explicit yes/no instructions
//   - the [clawvisor:approval=<id>] footer so the augmenter on later
//     turns can correlate this prompt with its outcome
//
// approvalID, when non-empty, is appended as the footer. Empty
// approvalID is supported (defensive — a missing footer just means
// the augmenter skips the correlation for this turn) but production
// always passes a real ID.
func renderScopeDriftOneOffPrompt(drift ScopeDrift, approvalID string) string {
	var b strings.Builder
	b.WriteString("Clawvisor: the agent requested a one-off execution of a tool call that's outside the active task scope.\n\n")

	target := strings.TrimSpace(drift.Service)
	if action := strings.TrimSpace(drift.Action); action != "" {
		if target == "" {
			target = action
		} else {
			target = target + "." + action
		}
	}
	if target != "" {
		b.WriteString("Tool\n  ")
		b.WriteString(sanitizeUserText(target))
		b.WriteString("\n\n")
	}

	if reason := strings.TrimSpace(drift.ReasonText); reason != "" {
		b.WriteString("Block reason\n  ")
		b.WriteString(wrapForPrompt(sanitizeUserText(reason), 80, "  "))
		b.WriteString("\n\n")
	}

	note := strings.TrimSpace(drift.AgentNote)
	if note == "" {
		note = "(no rationale supplied by the agent)"
	}
	b.WriteString("Agent rationale\n  ")
	b.WriteString(wrapForPrompt(sanitizeUserText(note), 80, "  "))
	b.WriteString("\n\n")

	b.WriteString("Approving authorizes this single call — no task scope is created, and the agent will not be permitted to repeat the call without another approval.\n\n")
	b.WriteString("Reply `yes` or `y` to approve this one-off, `no` or `n` to deny.")
	b.WriteString(approvalIDFooter(approvalID))
	return b.String()
}
