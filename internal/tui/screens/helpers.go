package screens

import (
	"fmt"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/tui"
	"github.com/clawvisor/clawvisor/internal/tui/client"
)

func formatTaskDetail(t *client.Task) string {
	var b strings.Builder

	b.WriteString(tui.StyleDim.Render("Status:    ") + t.Status + "\n")
	b.WriteString(tui.StyleDim.Render("Lifetime:  ") + t.Lifetime + "\n")
	b.WriteString(tui.StyleDim.Render("Agent:     ") + t.AgentName + "\n")
	b.WriteString(tui.StyleDim.Render("Created:   ") + t.CreatedAt.Format(time.RFC3339) + "\n")
	if t.ExpiresAt != nil {
		b.WriteString(tui.StyleDim.Render("Expires:   ") + t.ExpiresAt.Format(time.RFC3339) + "\n")
	}
	b.WriteString("\n")

	if len(t.AuthorizedActions) > 0 {
		b.WriteString(tui.StyleBold.Render("Authorized Actions") + "\n")
		for _, a := range t.AuthorizedActions {
			auto := "per-request"
			if a.AutoExecute {
				auto = "auto"
			}
			b.WriteString(fmt.Sprintf("  %s/%s (%s)", a.Service, a.Action, auto))
			if a.ExpectedUse != "" {
				b.WriteString("  — " + a.ExpectedUse)
			}
			b.WriteString("\n")
		}
	}

	if t.PendingAction != nil {
		b.WriteString("\n" + tui.StyleAmber.Render("Pending Expansion") + "\n")
		b.WriteString(fmt.Sprintf("  %s/%s\n", t.PendingAction.Service, t.PendingAction.Action))
		if t.PendingReason != "" {
			b.WriteString("  Reason: " + t.PendingReason + "\n")
		}
	}

	return b.String()
}

func formatApprovalDetail(a *client.QueueApproval, created time.Time) string {
	var b strings.Builder

	b.WriteString(tui.StyleDim.Render("Service:    ") + a.Service + "\n")
	b.WriteString(tui.StyleDim.Render("Action:     ") + a.Action + "\n")
	b.WriteString(tui.StyleDim.Render("Request ID: ") + a.RequestID + "\n")
	b.WriteString(tui.StyleDim.Render("Created:    ") + created.Format(time.RFC3339) + "\n")

	if a.Reason != "" {
		b.WriteString("\n" + tui.StyleBold.Render("Reason") + "\n")
		b.WriteString("  " + a.Reason + "\n")
	}

	if len(a.Params) > 0 {
		b.WriteString("\n" + tui.StyleBold.Render("Parameters") + "\n")
		for k, v := range a.Params {
			b.WriteString(fmt.Sprintf("  %s: %v\n", k, v))
		}
	}

	return b.String()
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d min ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}
