package screens

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/clawvisor/clawvisor/internal/tui"
	"github.com/clawvisor/clawvisor/internal/tui/client"
)

// ── Messages ────────────────────────────────────────────────────────────────

type auditDataMsg struct {
	entries []client.AuditEntry
	total   int
}

type auditDetailMsg struct {
	entry *client.AuditEntry
	err   error
}

// ── Model ───────────────────────────────────────────────────────────────────

type ActivityScreen struct {
	client  *client.Client
	entries []client.AuditEntry
	total   int
	cursor  int
	offset  int
	width   int
	height  int
	loading bool
	err     error
	detail  Detail
	filter  client.AuditFilter
}

func NewActivityScreen(c *client.Client) *ActivityScreen {
	return &ActivityScreen{
		client: c,
		detail: NewDetail(),
		filter: client.AuditFilter{Limit: 50},
	}
}

func (s *ActivityScreen) Init() tea.Cmd {
	s.loading = true
	return s.fetchAudit()
}

func (s *ActivityScreen) Update(msg tea.Msg) (tui.ScreenModel, tea.Cmd) {
	var cmds []tea.Cmd

	if s.detail.Visible() {
		d, cmd := s.detail.Update(msg)
		s.detail = d
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if _, isKey := msg.(tea.KeyMsg); isKey {
			return s, tea.Batch(cmds...)
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		s.height = msg.Height
		s.detail = s.detail.SetSize(msg.Width, msg.Height)

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, tui.ListNavKeys.Up):
			if s.cursor > 0 {
				s.cursor--
			}
		case key.Matches(msg, tui.ListNavKeys.Down):
			if s.cursor < len(s.entries)-1 {
				s.cursor++
			}
		case key.Matches(msg, tui.ListNavKeys.Enter):
			return s, s.loadDetail()
		case key.Matches(msg, tui.Keys.Refresh):
			return s, s.fetchAudit()
		case key.Matches(msg, key.NewBinding(key.WithKeys("pgdown"))):
			if s.offset+s.filter.Limit < s.total {
				s.offset += s.filter.Limit
				s.filter.Offset = s.offset
				s.cursor = 0
				return s, s.fetchAudit()
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("pgup"))):
			if s.offset > 0 {
				s.offset -= s.filter.Limit
				if s.offset < 0 {
					s.offset = 0
				}
				s.filter.Offset = s.offset
				s.cursor = 0
				return s, s.fetchAudit()
			}
		}

	case tui.TickMsg:
		return s, s.fetchAudit()

	case auditDataMsg:
		s.loading = false
		s.err = nil
		s.entries = msg.entries
		s.total = msg.total
		if s.cursor >= len(s.entries) {
			s.cursor = max(0, len(s.entries)-1)
		}
		cmds = append(cmds, tui.ConnState(true))

	case auditDetailMsg:
		if msg.err != nil {
			s.err = msg.err
		} else if msg.entry != nil {
			s.detail.Show(
				"Audit: "+msg.entry.Service+"/"+msg.entry.Action,
				formatAuditDetail(msg.entry),
			)
		}

	case tui.ErrMsg:
		s.err = msg.Err
		s.loading = false
		cmds = append(cmds, tui.ConnState(false))
	}

	return s, tea.Batch(cmds...)
}

func (s *ActivityScreen) View() string {
	if s.detail.Visible() {
		return s.detail.View()
	}

	var b strings.Builder

	header := lipgloss.NewStyle().Foreground(tui.ColorWhite).Bold(true)
	b.WriteString(header.Render("RECENT ACTIVITY"))
	b.WriteString("\n")
	b.WriteString(tui.StyleDim.Render(strings.Repeat("─", min(80, s.contentWidth()))))
	b.WriteString("\n\n")

	if s.err != nil {
		b.WriteString(tui.StyleRed.Render("Error: " + s.err.Error()) + "\n")
	}

	if s.loading && len(s.entries) == 0 {
		b.WriteString(tui.StyleDim.Render("  Loading..."))
		return b.String()
	}

	if len(s.entries) == 0 {
		b.WriteString(tui.StyleDim.Render("  No activity yet."))
		return b.String()
	}

	// Responsive column widths based on available content area.
	cw := s.contentWidth()
	const (
		markerW = 2
		timeW   = 10
		statusW = 12
	)
	flexW := cw - markerW - timeW - statusW
	if flexW < 24 {
		flexW = 24
	}
	serviceW := flexW * 3 / 5
	actionW := flexW - serviceW

	// Table header.
	colTime := lipgloss.NewStyle().Width(timeW).MaxWidth(timeW)
	colService := lipgloss.NewStyle().Width(serviceW).MaxWidth(serviceW)
	colAction := lipgloss.NewStyle().Width(actionW).MaxWidth(actionW)
	colStatus := lipgloss.NewStyle().Width(statusW).MaxWidth(statusW)

	headerLine := tui.StyleTableHeader.Render(
		colTime.Render("TIME") +
			colService.Render("SERVICE") +
			colAction.Render("ACTION") +
			colStatus.Render("STATUS"),
	)
	b.WriteString(headerLine + "\n")

	for i, entry := range s.entries {
		selected := i == s.cursor
		marker := "  "
		if selected {
			marker = tui.StyleBrand.Render("> ")
		}

		timeStr := entry.Timestamp.Local().Format("3:04 PM")

		var statusIcon string
		switch entry.Outcome {
		case "executed":
			statusIcon = tui.StyleGreen.Render("✓ exec")
		case "denied":
			statusIcon = tui.StyleRed.Render("✗ deny")
		case "pending":
			statusIcon = tui.StyleAmber.Render("⏳ pend")
		case "timeout":
			statusIcon = tui.StyleDim.Render("⏱ time")
		case "error":
			statusIcon = tui.StyleRed.Render("! err")
		default:
			statusIcon = tui.StyleDim.Render(entry.Outcome)
		}

		line := marker +
			colTime.Render(timeStr) +
			colService.Render(entry.Service) +
			colAction.Render(entry.Action) +
			colStatus.Render(statusIcon)

		b.WriteString(line + "\n")
	}

	// Pagination info.
	if s.total > s.filter.Limit {
		page := s.offset/s.filter.Limit + 1
		totalPages := (s.total + s.filter.Limit - 1) / s.filter.Limit
		b.WriteString("\n")
		b.WriteString(tui.StyleDim.Render(fmt.Sprintf("  Page %d/%d (%d total)  [pgup/pgdn] Navigate", page, totalPages, s.total)))
	}

	return b.String()
}

func (s *ActivityScreen) ShortHelp() []string {
	return []string{
		tui.StyleStatusKey.Render("[enter]") + tui.StyleStatusBar.Render(" Details"),
		tui.StyleStatusKey.Render("[pgup/pgdn]") + tui.StyleStatusBar.Render(" Page"),
	}
}

// ── Actions ─────────────────────────────────────────────────────────────────

func (s *ActivityScreen) fetchAudit() tea.Cmd {
	c := s.client
	f := s.filter
	return func() tea.Msg {
		resp, err := c.GetAudit(f)
		if err != nil {
			return tui.ErrMsg{Err: err}
		}
		return auditDataMsg{entries: resp.Entries, total: resp.Total}
	}
}

func (s *ActivityScreen) loadDetail() tea.Cmd {
	if s.cursor >= len(s.entries) {
		return nil
	}
	entry := s.entries[s.cursor]
	c := s.client
	return func() tea.Msg {
		full, err := c.GetAuditEntry(entry.ID)
		if err != nil {
			return auditDetailMsg{err: err}
		}
		return auditDetailMsg{entry: full}
	}
}

func (s *ActivityScreen) contentWidth() int {
	w := s.width - 26
	if w < 40 {
		w = 40
	}
	return w
}

// ── Formatting ──────────────────────────────────────────────────────────────

func formatAuditDetail(e *client.AuditEntry) string {
	var b strings.Builder

	b.WriteString(tui.StyleDim.Render("Service:     ") + e.Service + "\n")
	b.WriteString(tui.StyleDim.Render("Action:      ") + e.Action + "\n")
	b.WriteString(tui.StyleDim.Render("Outcome:     ") + e.Outcome + "\n")
	b.WriteString(tui.StyleDim.Render("Decision:    ") + e.Decision + "\n")
	b.WriteString(tui.StyleDim.Render("Request ID:  ") + e.RequestID + "\n")
	b.WriteString(tui.StyleDim.Render("Timestamp:   ") + e.Timestamp.Local().Format("2006-01-02 15:04:05") + "\n")
	b.WriteString(tui.StyleDim.Render("Duration:    ") + fmt.Sprintf("%dms", e.DurationMs) + "\n")

	if e.TaskID != "" {
		b.WriteString(tui.StyleDim.Render("Task ID:     ") + e.TaskID + "\n")
	}
	if e.AgentID != "" {
		b.WriteString(tui.StyleDim.Render("Agent ID:    ") + e.AgentID + "\n")
	}

	if e.Reason != "" {
		b.WriteString("\n" + tui.StyleBold.Render("Reason") + "\n")
		b.WriteString("  " + e.Reason + "\n")
	}

	if len(e.ParamsSafe) > 0 {
		b.WriteString("\n" + tui.StyleBold.Render("Parameters") + "\n")
		for k, v := range e.ParamsSafe {
			b.WriteString(fmt.Sprintf("  %s: %v\n", k, v))
		}
	}

	if e.SafetyFlagged {
		b.WriteString("\n" + tui.StyleRed.Render("Safety Flagged") + "\n")
		b.WriteString("  " + e.SafetyReason + "\n")
	}

	if e.Verification != nil {
		v := e.Verification
		b.WriteString("\n" + tui.StyleBold.Render("Intent Verification") + "\n")
		allowed := tui.StyleGreen.Render("allowed")
		if !v.Allow {
			allowed = tui.StyleRed.Render("denied")
		}
		b.WriteString("  Result: " + allowed + "\n")
		b.WriteString("  Model: " + v.Model + "\n")
		b.WriteString(fmt.Sprintf("  Latency: %dms", v.LatencyMs))
		if v.Cached {
			b.WriteString(" (cached)")
		}
		b.WriteString("\n")
		b.WriteString("  Explanation: " + v.Explanation + "\n")
	}

	if e.ErrorMsg != "" {
		b.WriteString("\n" + tui.StyleRed.Render("Error") + "\n")
		b.WriteString("  " + e.ErrorMsg + "\n")
	}

	return b.String()
}
