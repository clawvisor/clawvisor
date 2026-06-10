package screens

import (
	"encoding/json"
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
	if badge := riskBadge(t.RiskLevel); badge != "" {
		b.WriteString(tui.StyleDim.Render("Risk:      ") + badge + "\n")
	}
	b.WriteString("\n")

	if len(t.RiskDetails) > 0 {
		var ra client.RiskAssessment
		if json.Unmarshal(t.RiskDetails, &ra) == nil && ra.Explanation != "" {
			b.WriteString(tui.StyleBold.Render("Risk Assessment") + "\n")
			b.WriteString("  " + ra.Explanation + "\n")
			if len(ra.Factors) > 0 {
				for _, f := range ra.Factors {
					b.WriteString("  • " + f + "\n")
				}
			}
			if len(ra.Conflicts) > 0 {
				b.WriteString("\n")
				for _, c := range ra.Conflicts {
					b.WriteString("  " + tui.StyleRed.Render("✗") + " " + c.Description)
					if c.Severity != "" {
						b.WriteString(" (" + c.Severity + ")")
					}
					b.WriteString("\n")
				}
			}
			if ra.Model != "" {
				b.WriteString(tui.StyleDim.Render(fmt.Sprintf("  model: %s  latency: %dms", ra.Model, ra.LatencyMs)) + "\n")
			}
			b.WriteString("\n")
		}
	}

	if len(t.AuthorizedActions) > 0 {
		b.WriteString(tui.StyleBold.Render("Authorized Actions") + "\n")
		for _, a := range t.AuthorizedActions {
			auto := "per-request"
			if a.AutoExecute {
				auto = "auto"
			}
			b.WriteString(fmt.Sprintf("  %s/%s (%s)", a.Service, a.Action, auto))
			// Fall back to the expansion rationale when no create-time
			// expected_use was supplied. Without this, a derived
			// authorized_action (materialized from an expansion's
			// ExpectedTool) renders blank even though the agent
			// declared a per-entry why on expand.
			if note := a.ExpectedUse; note != "" {
				b.WriteString("  — " + note)
			} else if a.ExpansionRationale != "" {
				b.WriteString("  — " + a.ExpansionRationale)
			}
			b.WriteString("\n")
		}
	}

	if t.PendingExpansion != nil {
		b.WriteString("\n" + tui.StyleAmber.Render("Pending Expansion") + "\n")
		writePendingExpansionSummary(&b, t)
	}

	return b.String()
}

// writePendingExpansionSummary renders a multi-line view of an in-flight
// expansion: each requested addition (tool / egress / credential) as a
// bullet, marked as a NEW entry or a REPLACE-by-name against the parent
// envelope (with a was/now diff for replacements so the reviewer sees
// what is genuinely changing, not just the new value). Derived gateway
// scopes (tool_name shaped as service:action) carry an auto-execute
// marker from the server's pre-computed PendingDerivedActions.
//
// Decode errors leave the relevant section out rather than panicking —
// display surfaces are read-only and an unrenderable pending row should
// still let the rest of the task details be inspected.
func writePendingExpansionSummary(b *strings.Builder, t *client.Task) {
	if t == nil || t.PendingExpansion == nil {
		return
	}
	pending := t.PendingExpansion

	// Parent envelope: case-insensitive key → prior why. Used to detect
	// replacements vs. new entries client-side.
	parentToolWhy := indexParentToolsByName(t.ExpectedTools)
	parentEgressWhy := indexParentEgressByHost(t.ExpectedEgress)
	parentCredWhy := indexParentCredentialsByKey(t.RequiredCredentials)

	// Derived gateway scopes (server-computed). Match by service:action
	// so we can stamp an "auto-execute" / "needs approval" marker next
	// to each derived ExpectedTool entry.
	derivedByKey := make(map[string]client.TaskAction, len(t.PendingDerivedActions))
	for _, a := range t.PendingDerivedActions {
		derivedByKey[strings.ToLower(a.Service+":"+a.Action)] = a
	}

	if len(pending.ExpectedTools) > 0 {
		var tools []struct {
			ToolName string `json:"tool_name"`
			Why      string `json:"why"`
		}
		if json.Unmarshal(pending.ExpectedTools, &tools) == nil {
			for _, tool := range tools {
				key := strings.ToLower(strings.TrimSpace(tool.ToolName))
				prefix := "  + "
				prior, replaced := parentToolWhy[key]
				if replaced {
					prefix = "  ~ "
				}
				line := prefix + tool.ToolName
				if marker := autoExecuteMarker(tool.ToolName, derivedByKey); marker != "" {
					line += "  " + marker
				}
				b.WriteString(line + "\n")
				if replaced && prior != tool.Why {
					b.WriteString("      was: " + prior + "\n")
					b.WriteString("      now: " + tool.Why + "\n")
				} else if tool.Why != "" {
					b.WriteString("      " + tool.Why + "\n")
				}
			}
		}
	}
	if len(pending.ExpectedEgress) > 0 {
		var egress []struct {
			Host string `json:"host"`
			Why  string `json:"why"`
		}
		if json.Unmarshal(pending.ExpectedEgress, &egress) == nil {
			for _, e := range egress {
				key := strings.ToLower(strings.TrimSpace(e.Host))
				prefix := "  + "
				prior, replaced := parentEgressWhy[key]
				if replaced {
					prefix = "  ~ "
				}
				b.WriteString(prefix + e.Host + "\n")
				if replaced && prior != e.Why {
					b.WriteString("      was: " + prior + "\n")
					b.WriteString("      now: " + e.Why + "\n")
				} else if e.Why != "" {
					b.WriteString("      " + e.Why + "\n")
				}
			}
		}
	}
	if len(pending.RequiredCredentials) > 0 {
		var creds []client.RequiredCredential
		if json.Unmarshal(pending.RequiredCredentials, &creds) == nil {
			for _, c := range creds {
				id := c.VaultItemID
				if id == "" {
					id = c.VaultItemHandle
				}
				key := credentialKey(c)
				prefix := "  + "
				prior, replaced := parentCredWhy[key]
				if replaced {
					prefix = "  ~ "
				}
				b.WriteString(prefix + id + "\n")
				if replaced && prior != c.Why {
					b.WriteString("      was: " + prior + "\n")
					b.WriteString("      now: " + c.Why + "\n")
				} else if c.Why != "" {
					b.WriteString("      " + c.Why + "\n")
				}
			}
		}
	}
	if pending.Reason != "" {
		b.WriteString("  Reason: " + pending.Reason + "\n")
	}
}

// indexParentToolsByName builds a case-insensitive lookup from tool name
// to the parent's why. Used by writePendingExpansionSummary to detect
// replacements without re-running the merge logic.
func indexParentToolsByName(parent []client.ExpectedTool) map[string]string {
	out := make(map[string]string, len(parent))
	for _, t := range parent {
		key := strings.ToLower(strings.TrimSpace(t.ToolName))
		if key == "" {
			continue
		}
		out[key] = t.Why
	}
	return out
}

func indexParentEgressByHost(parent []client.ExpectedEgress) map[string]string {
	out := make(map[string]string, len(parent))
	for _, e := range parent {
		key := strings.ToLower(strings.TrimSpace(e.Host))
		if key == "" {
			continue
		}
		out[key] = e.Why
	}
	return out
}

// indexParentCredentialsByKey is the credential analogue of
// indexParentToolsByName: kind-scoped (id vs handle) so a value
// collision across kinds doesn't masquerade as a replace. Mirrors the
// envelope merger's canonical key.
func indexParentCredentialsByKey(parent []client.RequiredCredential) map[string]string {
	out := make(map[string]string, len(parent))
	for _, c := range parent {
		key := credentialKey(c)
		if key == "" {
			continue
		}
		out[key] = c.Why
	}
	return out
}

// credentialKey mirrors the envelope merger's requiredCredentialKey
// shape (kind:value, lowercased). Keeping the lookup key in lockstep
// with the server's dedup keeps the dashboard / TUI was/now diff
// labels exactly aligned with what the merge persists.
func credentialKey(c client.RequiredCredential) string {
	if id := strings.ToLower(strings.TrimSpace(c.VaultItemID)); id != "" {
		return "id:" + id
	}
	if handle := strings.ToLower(strings.TrimSpace(c.VaultItemHandle)); handle != "" {
		return "handle:" + handle
	}
	return ""
}

// autoExecuteMarker returns the auto-execute disposition tag for a tool
// entry that maps to a derived gateway scope. Local-only tools (no
// service:action shape) return "" so the renderer omits the marker.
//
// The mapping is server-supplied via PendingDerivedActions — clients
// don't need to know the RequiresHardcodedApproval table.
func autoExecuteMarker(toolName string, derived map[string]client.TaskAction) string {
	// idx is the colon's position inside the trimmed string, so the
	// "colon is the last rune" guard must compare against the trimmed
	// length too. An earlier version of this function compared idx
	// against len(toolName) (the untrimmed length), which off-by-Ns
	// past the trimmed terminator and silently let trailing-colon
	// inputs like "github:" slip past the guard with an empty action.
	trimmed := strings.TrimSpace(toolName)
	idx := strings.LastIndex(trimmed, ":")
	if idx <= 0 || idx == len(trimmed)-1 {
		return ""
	}
	key := strings.ToLower(trimmed)
	a, ok := derived[key]
	if !ok {
		return ""
	}
	if a.AutoExecute {
		return tui.StyleGreen.Render("[auto-execute]")
	}
	return tui.StyleAmber.Render("[needs per-call approval]")
}

func riskBadge(level string) string {
	switch level {
	case "low":
		return tui.StyleGreen.Render("low risk")
	case "medium":
		return tui.StyleAmber.Render("medium risk")
	case "high":
		return tui.StyleOrange.Render("high risk")
	case "critical":
		return tui.StyleRed.Render("critical risk")
	default:
		return ""
	}
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

func isHighRisk(level string) bool {
	return level == "high" || level == "critical"
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
