package llmproxy

import (
	"strings"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
)

// renderExpansionApprovalPrompt builds the inline yes/no prompt the
// model substitutes in place of the synthetic tool_use_result for a
// POST /api/control/tasks/{id}/expand?surface=inline. Parallel to
// renderTaskApprovalPromptWithRisk for task creation; the shape is
// intentionally close so a user mid-conversation reads continuity
// across "task" and "scope" approvals.
//
// parentPurpose is the original task's purpose (echoed back so the
// reviewer knows which task this expansion attaches to). It is
// agent-supplied text but came from a previously-approved task, so
// it has already passed an inline review.
//
// approvalID, when non-empty, is appended as a parseable footer the
// history augmenter consumes — exactly the same shape the task-
// creation prompt uses, so the augmentation pipeline can share the
// outcome-store keying. Approval ID extraction goes through
// extractApprovalIDFromPrompt unchanged.
func renderExpansionApprovalPrompt(additions *runtimetasks.Envelope, reason, parentPurpose, parentTaskID, approvalID string) string {
	suffix := approvalIDFooter(approvalID)
	if additions == nil {
		return "Clawvisor wants to expand a task's scope.\n\nReply `yes` or `y` to authorize, `no` or `n` to cancel." + suffix
	}

	var b strings.Builder
	b.WriteString("Clawvisor wants to expand the scope of an existing task:\n\n")
	if purpose := sanitizeUserText(strings.TrimSpace(parentPurpose)); purpose != "" {
		b.WriteString("Task\n  ")
		b.WriteString(wrapForPrompt(purpose, 80, "    "))
		if id := strings.TrimSpace(parentTaskID); id != "" {
			b.WriteString(" (")
			b.WriteString(id)
			b.WriteString(")")
		}
	} else if id := strings.TrimSpace(parentTaskID); id != "" {
		b.WriteString("Task\n  ")
		b.WriteString(id)
	}

	if r := sanitizeUserText(strings.TrimSpace(reason)); r != "" {
		b.WriteString("\n\nReason\n  ")
		b.WriteString(wrapForPrompt(r, 80, "    "))
	}

	if len(additions.ExpectedTools) > 0 {
		b.WriteString("\n\nAdditional tools")
		for _, tool := range additions.ExpectedTools {
			name := sanitizeUserText(strings.TrimSpace(tool.ToolName))
			if name == "" {
				continue
			}
			b.WriteString("\n  • ")
			b.WriteString(name)
			if why := sanitizeUserText(strings.TrimSpace(tool.Why)); why != "" {
				b.WriteString(" — ")
				b.WriteString(wrapForPrompt(why, 80, "      "))
			}
		}
	}

	if len(additions.ExpectedEgress) > 0 {
		b.WriteString("\n\nAdditional egress")
		for _, eg := range additions.ExpectedEgress {
			host := sanitizeUserText(strings.TrimSpace(eg.Host))
			if host == "" {
				continue
			}
			b.WriteString("\n  • ")
			b.WriteString(host)
			if why := sanitizeUserText(strings.TrimSpace(eg.Why)); why != "" {
				b.WriteString(" — ")
				b.WriteString(wrapForPrompt(why, 80, "      "))
			}
		}
	}

	if len(additions.RequiredCredentials) > 0 {
		b.WriteString("\n\nAdditional credentials")
		for _, cred := range additions.RequiredCredentials {
			name := sanitizeUserText(strings.TrimSpace(cred.VaultItemID))
			if name == "" {
				name = sanitizeUserText(strings.TrimSpace(cred.VaultItemHandle))
			}
			if name == "" {
				continue
			}
			b.WriteString("\n  • ")
			b.WriteString(name)
			if why := sanitizeUserText(strings.TrimSpace(cred.Why)); why != "" {
				b.WriteString(" — ")
				b.WriteString(wrapForPrompt(why, 80, "      "))
			}
		}
	}

	b.WriteString("\n\nReply `yes` or `y` to authorize, `no` or `n` to cancel.")
	b.WriteString(suffix)
	return b.String()
}

// inlineExpansionApprovedReplyAugmentationContext is the body the
// rewrite path emits as the substituted "approve" user turn after a
// successful inline expansion. Parallels inlineApprovedReplyAugmentationContext
// for task creation — the message tells the model that scope was
// added to the existing task and not to re-emit the expand POST.
//
// We omit per-entry details (which tool, which host) for the same
// reason the task-creation augmentation does: the persistent
// augmenter on later turns can't reconstruct those fields without a
// DB lookup, and drift between the one-shot rewrite and the
// later-turn augmentation hurts more than the missing specifics
// help. The model can re-fetch the task via /control/tasks if it
// needs the merged shape.
func inlineExpansionApprovedReplyAugmentationContext(taskID string, credentials []InlineTaskCredentialPlaceholder) string {
	var b strings.Builder
	b.WriteString("Task scope was expanded and approved by the user. The new tools / egress / credentials are now part of task ")
	if id := strings.TrimSpace(taskID); id != "" {
		b.WriteString(id)
	} else {
		b.WriteString("the active task")
	}
	b.WriteString(". Proceed with your next tool_use(s) using the expanded scope. Do NOT POST /control/tasks/<id>/expand again for the same delta.")
	if len(credentials) > 0 {
		b.WriteString(" Credential placeholders minted for the expansion:")
		for _, cred := range credentials {
			if strings.TrimSpace(cred.Placeholder) == "" {
				continue
			}
			name := strings.TrimSpace(cred.VaultItemID)
			if name == "" {
				name = strings.TrimSpace(cred.ServiceID)
			}
			if name == "" {
				name = "credential"
			}
			b.WriteString(" ")
			b.WriteString(name)
			b.WriteString("=")
			b.WriteString(cred.Placeholder)
			b.WriteString(";")
		}
		b.WriteString(" use these exact placeholder values in Authorization headers or curl arguments.")
	}
	return Render(NoticeKindTaskApproved, b.String())
}

// renderInlineExpansionDenyReply mirrors renderInlineTaskDenyReply for
// scope expansion. The model must understand the expansion was
// refused and not retry the same delta.
func renderInlineExpansionDenyReply() string {
	return Render(NoticeKindTaskDenied, "The user denied the scope-expansion request. The parent task's existing scope is unchanged; do not retry the same expansion. Acknowledge the denial; if the user still wants the work done, ask whether they prefer a narrower expansion or a different approach.")
}

// renderInlineExpansionAlreadyTerminalReply is the chat-side reply
// when the user's "approve" reaches us after the expansion was
// resolved on another surface (dashboard / notifier approve or deny,
// or a sweep). Mirrors renderInlineTaskAlreadyTerminalReply.
func renderInlineExpansionAlreadyTerminalReply(status string) string {
	verb := "resolved"
	switch status {
	case "active":
		verb = "approved"
	case "denied":
		verb = "denied"
	case "expired":
		verb = "let lapse"
	case "revoked":
		verb = "revoked"
	}
	return Render(NoticeKindTaskError, "Scope expansion was already "+verb+" on another surface (dashboard or notifier) before your approval landed. Re-fetch /control/tasks to see the current scope; do NOT re-POST the same expand body.")
}
