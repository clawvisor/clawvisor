package llmproxy

import (
	"strings"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
)

// renderTaskApprovalPrompt builds the inline approve/deny prompt the model
// substitutes in place of the synthetic task_use_result for a model-emitted
// POST /control/tasks when the user is mid-flight on an inline task gesture
// (StageAwaitingTaskApproval).
//
// The output is plain text in the same shape as approvalPrompt — the harness
// renders it verbatim, so the user sees a continuation of the same approval
// conversation rather than a context switch to the dashboard.
//
// Fields rendered:
//   - purpose (wrapped at 80 cols)
//   - expected_tools_json[].tool_name + .why (bullet list)
//   - required_credentials_json[].vault_item_id / vault_item_handle + .why (bullet list)
//   - intent_verification_mode (default "strict")
//   - lifetime humanized ("until session ends" / "always")
//   - expires_in_seconds humanized ("10 min" / "1 hour")
//
// Malformed or empty input falls back to a one-line summary instead of
// raw JSON — never leak unparsed input back at the user.
//
// approvalID, when non-empty, is appended as a parseable footer
// (InlineApprovalIDMarker) so the history augmenter on subsequent turns
// can correlate this prompt with the per-approval outcome recorded by
// RewriteInlineTaskApprovalReply. Without that correlation the
// augmenter would have no way to tell a successful approval apart from
// a failed one when both leave only a bare "approve" in conversation
// history.
func renderTaskApprovalPrompt(req *runtimetasks.TaskCreateRequest, approvalID string) string {
	suffix := approvalIDFooter(approvalID)
	if req == nil {
		return "Clawvisor wants to create a task.\n\nReply `approve` to authorize, `deny` to cancel." + suffix
	}
	purpose := strings.TrimSpace(req.Purpose)
	if purpose == "" {
		return "Clawvisor wants to create a task: unnamed.\n\nReply `approve` to authorize, `deny` to cancel." + suffix
	}

	var b strings.Builder
	b.WriteString("Clawvisor wants to create a task to cover this work:\n\n")
	b.WriteString("Purpose\n  ")
	b.WriteString(wrapForPrompt(purpose, 80, "  "))

	if len(req.ExpectedTools) > 0 {
		b.WriteString("\n\nTools requested")
		for _, tool := range req.ExpectedTools {
			name := strings.TrimSpace(tool.ToolName)
			if name == "" {
				continue
			}
			b.WriteString("\n  • ")
			b.WriteString(name)
			if why := strings.TrimSpace(tool.Why); why != "" {
				b.WriteString(" — ")
				b.WriteString(wrapForPrompt(why, 80, "    "))
			}
		}
	}

	if len(req.ExpectedEgress) > 0 {
		b.WriteString("\n\nNetwork egress")
		for _, eg := range req.ExpectedEgress {
			host := strings.TrimSpace(eg.Host)
			if host == "" {
				continue
			}
			b.WriteString("\n  • ")
			b.WriteString(host)
			if why := strings.TrimSpace(eg.Why); why != "" {
				b.WriteString(" — ")
				b.WriteString(wrapForPrompt(why, 80, "    "))
			}
		}
	}

	if len(req.RequiredCredentials) > 0 {
		b.WriteString("\n\nCredentials requested")
		for _, cred := range req.RequiredCredentials {
			name := strings.TrimSpace(cred.VaultItemID)
			if name == "" {
				name = strings.TrimSpace(cred.VaultItemHandle)
			}
			if name == "" {
				continue
			}
			b.WriteString("\n  • ")
			b.WriteString(name)
			if why := strings.TrimSpace(cred.Why); why != "" {
				b.WriteString(" — ")
				b.WriteString(wrapForPrompt(why, 80, "    "))
			}
		}
	}

	mode := strings.TrimSpace(req.IntentVerificationMode)
	if mode == "" {
		mode = "strict"
	}
	lifetime := humanizeLifetime(req.Lifetime)
	expires := humanizeExpiresIn(req.ExpiresInSeconds)

	b.WriteString("\n\nVerification: ")
	b.WriteString(mode)
	if lifetime != "" {
		b.WriteString("   Lifetime: ")
		b.WriteString(lifetime)
	}
	if expires != "" {
		b.WriteString("   Expires: ")
		b.WriteString(expires)
	}

	b.WriteString("\n\nApproving will create this task and run the original tool call.\n")
	b.WriteString("Reply `approve` to authorize, `deny` to cancel.")
	b.WriteString(suffix)
	return b.String()
}

// InlineApprovalIDMarker is the prefix of the footer line that
// renderTaskApprovalPrompt appends and that the history augmenter
// parses. Format: "\n\n[clawvisor:approval=<id>]".
const InlineApprovalIDMarker = "[clawvisor:approval="

func approvalIDFooter(approvalID string) string {
	if approvalID == "" {
		return ""
	}
	return "\n\n" + InlineApprovalIDMarker + approvalID + "]"
}

// extractApprovalIDFromPrompt pulls the approval ID out of an assistant
// prompt that ends with the InlineApprovalIDMarker footer. Returns ""
// if the marker is absent or malformed — the augmenter treats that as
// "outcome unknown" and skips the augmentation rather than guessing.
func extractApprovalIDFromPrompt(text string) string {
	idx := strings.LastIndex(text, InlineApprovalIDMarker)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(InlineApprovalIDMarker):]
	end := strings.IndexByte(rest, ']')
	if end <= 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// humanizeLifetime maps the task lifetime to a short user-facing phrase.
// Defaults to "until session ends" for empty/"session"; "always" for
// "standing". Unknown values pass through as-is.
func humanizeLifetime(lifetime string) string {
	switch strings.TrimSpace(lifetime) {
	case "", "session":
		return "until session ends"
	case "standing":
		return "always"
	default:
		return lifetime
	}
}

// humanizeExpiresIn maps a seconds duration to a short phrase. Returns
// "" when the field is unset (which means "use the daemon default"), so
// the renderer can omit the Expires line entirely.
func humanizeExpiresIn(seconds int) string {
	if seconds <= 0 {
		return ""
	}
	switch {
	case seconds%3600 == 0:
		hours := seconds / 3600
		if hours == 1 {
			return "1 hour"
		}
		return itoaShort(hours) + " hours"
	case seconds%60 == 0:
		mins := seconds / 60
		if mins == 1 {
			return "1 min"
		}
		return itoaShort(mins) + " min"
	default:
		return itoaShort(seconds) + " sec"
	}
}

// wrapForPrompt soft-wraps text at column width, breaking on word
// boundaries. Continuation lines get the given indent. The intent is
// readability inside a fixed-width terminal — we deliberately keep this
// simple rather than pulling in a full text-wrapping dependency.
func wrapForPrompt(text string, width int, indent string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if width <= len(indent)+1 {
		return text
	}
	limit := width - len(indent)
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}
	var b strings.Builder
	lineLen := 0
	first := true
	for _, w := range words {
		switch {
		case first:
			b.WriteString(w)
			lineLen = len(w)
			first = false
		case lineLen+1+len(w) > limit:
			b.WriteString("\n")
			b.WriteString(indent)
			b.WriteString(w)
			lineLen = len(w)
		default:
			b.WriteString(" ")
			b.WriteString(w)
			lineLen += 1 + len(w)
		}
	}
	return b.String()
}

// itoaShort is a tiny strconv.Itoa replacement; lets us avoid importing
// strconv in this file for one call site.
func itoaShort(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
