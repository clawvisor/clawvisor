package llmproxy

import (
	"strings"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
)

// renderTaskApprovalPrompt builds the inline yes/no prompt the model
// substitutes in place of the synthetic task_use_result for a model-emitted
// POST /api/control/tasks when the user is mid-flight on an inline task gesture
// (StageAwaitingTaskApproval).
//
// The output is plain text in the same shape as approvalPrompt — the harness
// renders it verbatim, so the user sees a continuation of the same approval
// conversation rather than a context switch to the dashboard.
//
// Fields rendered:
//   - purpose (wrapped at 80 cols)
//   - expected_tools[].tool_name + .why (bullet list)
//   - required_credentials[].vault_item_id / vault_item_handle + .why (bullet list)
//   - assessed risk level + explanation when available (with 🟢/🟡/🟠/🔴 prefix)
//   - intent_verification_mode (only when non-strict — strict is default and noise)
//   - duration: "Duration: 30 min" for session tasks (using daemon default
//     when unset) or "Lifetime: always" for standing tasks
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
	return renderTaskApprovalPromptWithRisk(req, approvalID, nil)
}

func renderTaskApprovalPromptWithRisk(req *runtimetasks.TaskCreateRequest, approvalID string, risk *taskrisk.RiskAssessment) string {
	suffix := approvalIDFooter(approvalID)
	if req == nil {
		return "Clawvisor wants to create a task.\n\nReply `yes` or `y` to authorize, `no` or `n` to cancel." + suffix
	}
	purpose := strings.TrimSpace(req.Purpose)
	if purpose == "" {
		return "Clawvisor wants to create a task: unnamed.\n\nReply `yes` or `y` to authorize, `no` or `n` to cancel." + suffix
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

	if risk != nil && strings.TrimSpace(risk.RiskLevel) != "" {
		level := strings.TrimSpace(risk.RiskLevel)
		b.WriteString("\n\nRisk")
		b.WriteString("\n  ")
		if emoji := riskEmoji(level); emoji != "" {
			b.WriteString(emoji)
			b.WriteString(" ")
		}
		b.WriteString(level)
		if explanation := strings.TrimSpace(risk.Explanation); explanation != "" {
			b.WriteString(" — ")
			b.WriteString(wrapForPrompt(explanation, 80, "    "))
		}
	}

	mode := strings.TrimSpace(req.IntentVerificationMode)
	durationLabel, durationValue := durationLine(req.Lifetime, req.ExpiresInSeconds)

	if (mode != "" && mode != "strict") || durationValue != "" {
		b.WriteString("\n\n")
		needsSep := false
		if mode != "" && mode != "strict" {
			b.WriteString("Verification: ")
			b.WriteString(mode)
			needsSep = true
		}
		if durationValue != "" {
			if needsSep {
				b.WriteString("   ")
			}
			b.WriteString(durationLabel)
			b.WriteString(": ")
			b.WriteString(durationValue)
		}
	}

	b.WriteString("\n\nReply `yes` or `y` to authorize, `no` or `n` to cancel.")
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

// defaultSessionTaskDurationSeconds mirrors pkg/config.TaskConfig's
// default_expiry_seconds (1800 = 30 min). Duplicated here rather than
// threaded through PostprocessConfig because the renderer is a UI
// concern and only needs an honest display fallback — the actual
// task-scope expiry binding happens in tasks_inline.go using the live
// config value. If an operator overrides the config default, the
// prompt may show "30 min" while the resolved scope uses a different
// number; that drift is acceptable for a status line.
const defaultSessionTaskDurationSeconds = 1800

// durationLine returns the (label, value) pair describing how long the
// task will be active once approved. The two lifetime modes render with
// different labels because they answer different user questions:
//
//   - session (the common case): "Duration: 30 min" — how long the
//     scope is usable. Empty expires_in_seconds falls back to the daemon
//     default so the user always sees a concrete number.
//   - standing: "Lifetime: always" — standing tasks reject
//     expires_in_seconds (see tasks_inline.go), so there is no duration
//     to show; the label conveys "no time cap" instead.
//
// Returns ("", "") only for unknown lifetime strings, so callers can
// omit the line entirely rather than printing something misleading.
func durationLine(lifetime string, expiresInSeconds int) (label, value string) {
	switch strings.TrimSpace(lifetime) {
	case "", "session":
		secs := expiresInSeconds
		if secs <= 0 {
			secs = defaultSessionTaskDurationSeconds
		}
		return "Duration", humanizeExpiresIn(secs)
	case "standing":
		return "Lifetime", "always"
	default:
		return "", ""
	}
}

// riskEmoji maps the LLM-assessed risk level to a traffic-light emoji
// prefix. "unknown" gets a neutral marker so the user can distinguish a
// parse-failed assessment from an unscored task. Unknown levels return
// "" so the level renders without a prefix rather than with a
// misleading one.
func riskEmoji(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "low":
		return "🟢"
	case "medium":
		return "🟡"
	case "high":
		return "🟠"
	case "critical":
		return "🔴"
	case "unknown":
		return "⚪"
	default:
		return ""
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
