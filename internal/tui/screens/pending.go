package screens

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/clawvisor/clawvisor/internal/tui"
	"github.com/clawvisor/clawvisor/internal/tui/client"
)

// ── Messages ────────────────────────────────────────────────────────────────

type queueDataMsg struct {
	items []client.QueueItem
	total int
}

type approvalDoneMsg struct {
	action string // "approved" or "denied"
	id     string
	err    error
}

// ── Model ───────────────────────────────────────────────────────────────────

type PendingScreen struct {
	client  *client.Client
	items   []client.QueueItem
	cursor  int
	width   int
	height  int
	loading bool
	err     error
	detail  Detail
	busy    string
}

func NewPendingScreen(c *client.Client) *PendingScreen {
	return &PendingScreen{
		client: c,
		detail: NewDetail(),
	}
}

func (s *PendingScreen) Init() tea.Cmd {
	s.loading = true
	return s.fetchQueue()
}

func (s *PendingScreen) Update(msg tea.Msg) (tui.ScreenModel, tea.Cmd) {
	var cmds []tea.Cmd

	// Detail overlay.
	if s.detail.Visible() {
		d, cmd := s.detail.Update(msg)
		s.detail = d
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if !s.detail.Visible() {
			return s, tea.Batch(cmds...)
		}
		// Only pass through non-key messages when detail is open.
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
			if s.cursor < len(s.items)-1 {
				s.cursor++
			}
		case key.Matches(msg, tui.PendingKeys.Approve):
			if s.busy != "" {
				return s, nil
			}
			s.busy = "Approving..."
			return s, s.approveSelected()
		case key.Matches(msg, tui.PendingKeys.Deny):
			if s.busy != "" {
				return s, nil
			}
			s.busy = "Denying..."
			return s, s.denySelected()
		case key.Matches(msg, tui.ListNavKeys.Enter):
			s.showDetail()
		case key.Matches(msg, tui.Keys.Refresh):
			return s, s.fetchQueue()
		}

	case tui.TickMsg:
		return s, s.fetchQueue()

	case queueDataMsg:
		s.loading = false
		s.err = nil
		s.items = msg.items
		if s.cursor >= len(s.items) {
			s.cursor = max(0, len(s.items)-1)
		}
		cmds = append(cmds, func() tea.Msg {
			return tui.PendingCountMsg(msg.total)
		})
		cmds = append(cmds, tui.ConnState(true))

	case approvalDoneMsg:
		s.busy = ""
		if msg.err != nil {
			s.err = msg.err
		} else {
			cmds = append(cmds, func() tea.Msg {
				return tui.StatusMsg(fmt.Sprintf("Item %s", msg.action))
			})
		}
		// Refresh after action.
		cmds = append(cmds, s.fetchQueue())

	case tui.ErrMsg:
		s.err = msg.Err
		s.loading = false
		cmds = append(cmds, tui.ConnState(false))
	}

	return s, tea.Batch(cmds...)
}

func (s *PendingScreen) View() string {
	var b strings.Builder

	header := lipgloss.NewStyle().
		Foreground(tui.ColorWhite).
		Bold(true)
	b.WriteString(header.Render("PENDING APPROVALS"))
	b.WriteString("\n")
	b.WriteString(tui.StyleDim.Render(strings.Repeat("─", min(60, s.contentWidth()))))
	b.WriteString("\n")

	if s.busy != "" {
		b.WriteString(tui.StyleAmber.Render(s.busy))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if s.err != nil {
		b.WriteString(tui.StyleRed.Render("Error: " + s.err.Error()))
		b.WriteString("\n")
	}

	if s.loading && len(s.items) == 0 {
		b.WriteString(tui.StyleDim.Render("  Loading..."))
		return b.String()
	}

	if len(s.items) == 0 {
		b.WriteString(tui.StyleDim.Render("  No pending items. All clear."))
		return b.String()
	}

	for i, item := range s.items {
		selected := i == s.cursor
		b.WriteString(s.renderItem(item, selected))
		b.WriteString("\n")
	}

	return b.String()
}

func (s *PendingScreen) ShortHelp() []string {
	return []string{
		tui.StyleStatusKey.Render("[a]") + tui.StyleStatusBar.Render(" Approve"),
		tui.StyleStatusKey.Render("[d]") + tui.StyleStatusBar.Render(" Deny"),
		tui.StyleStatusKey.Render("[enter]") + tui.StyleStatusBar.Render(" Details"),
	}
}

// ── Rendering ───────────────────────────────────────────────────────────────

func (s *PendingScreen) renderItem(item client.QueueItem, selected bool) string {
	marker := "  "
	if selected {
		marker = tui.StyleBrand.Render("> ")
	}

	switch item.Type {
	case "task":
		return s.renderTaskItem(item, marker)
	case "approval":
		return s.renderApprovalItem(item, marker)
	default:
		return marker + tui.StyleDim.Render("Unknown item type: "+item.Type)
	}
}

func (s *PendingScreen) renderTaskItem(item client.QueueItem, marker string) string {
	if item.Task == nil {
		return marker + tui.StyleDim.Render("Task (no data)")
	}
	t := item.Task

	label := "Task"
	if t.Status == "pending_scope_expansion" {
		label = "Scope expansion"
	}

	var b strings.Builder
	b.WriteString(marker)
	b.WriteString(tui.StyleAmber.Render(label+": "))
	b.WriteString(tui.StyleBold.Render(t.Purpose))
	b.WriteString("  ")
	b.WriteString(tui.StyleDim.Render(timeAgo(item.CreatedAt)))
	b.WriteString("\n")

	// Agent and lifetime.
	b.WriteString("    ")
	b.WriteString(tui.StyleDim.Render("Agent: "))
	b.WriteString(t.AgentName)
	if t.AgentName == "" {
		b.WriteString(t.AgentID[:8])
	}
	b.WriteString("  ")
	b.WriteString(tui.StyleDim.Render("Lifetime: "))
	b.WriteString(t.Lifetime)
	b.WriteString("\n")

	// Actions.
	if len(t.AuthorizedActions) > 0 {
		b.WriteString("    ")
		b.WriteString(tui.StyleDim.Render("Actions: "))
		var actions []string
		for _, a := range t.AuthorizedActions {
			desc := a.Service + "/" + a.Action
			if a.AutoExecute {
				desc += " (auto)"
			} else {
				desc += " (approval)"
			}
			actions = append(actions, desc)
		}
		b.WriteString(strings.Join(actions, ", "))
		b.WriteString("\n")
	}

	// Pending expansion action.
	if t.PendingAction != nil {
		b.WriteString("    ")
		b.WriteString(tui.StyleAmber.Render("New action: "))
		b.WriteString(t.PendingAction.Service + "/" + t.PendingAction.Action)
		if t.PendingReason != "" {
			b.WriteString("  ")
			b.WriteString(tui.StyleDim.Render("Reason: "))
			b.WriteString(t.PendingReason)
		}
		b.WriteString("\n")
	}

	b.WriteString("  " + tui.StyleDim.Render(strings.Repeat("─", min(60, s.contentWidth()-2))))

	return b.String()
}

func (s *PendingScreen) renderApprovalItem(item client.QueueItem, marker string) string {
	if item.Approval == nil {
		return marker + tui.StyleDim.Render("Request (no data)")
	}
	a := item.Approval

	var b strings.Builder
	b.WriteString(marker)
	b.WriteString(tui.StyleAmber.Render("Request: "))
	b.WriteString(a.Service + " / " + a.Action)
	b.WriteString("  ")
	b.WriteString(tui.StyleDim.Render(timeAgo(item.CreatedAt)))
	b.WriteString("\n")

	if a.Reason != "" {
		b.WriteString("    ")
		b.WriteString(tui.StyleDim.Render("Reason: "))
		b.WriteString(a.Reason)
		b.WriteString("\n")
	}

	b.WriteString("  " + tui.StyleDim.Render(strings.Repeat("─", min(60, s.contentWidth()-2))))

	return b.String()
}

// ── Actions ─────────────────────────────────────────────────────────────────

func (s *PendingScreen) approveSelected() tea.Cmd {
	if s.cursor >= len(s.items) {
		return nil
	}
	item := s.items[s.cursor]
	c := s.client

	return func() tea.Msg {
		var err error
		switch item.Type {
		case "approval":
			if item.Approval != nil {
				_, err = c.ApproveRequest(item.Approval.RequestID)
			}
		case "task":
			if item.Task != nil {
				if item.Task.Status == "pending_scope_expansion" {
					_, err = c.ApproveExpansion(item.Task.ID)
				} else {
					_, err = c.ApproveTask(item.Task.ID)
				}
			}
		}
		if err != nil {
			return approvalDoneMsg{err: err}
		}
		return approvalDoneMsg{action: "approved", id: item.ID}
	}
}

func (s *PendingScreen) denySelected() tea.Cmd {
	if s.cursor >= len(s.items) {
		return nil
	}
	item := s.items[s.cursor]
	c := s.client

	return func() tea.Msg {
		var err error
		switch item.Type {
		case "approval":
			if item.Approval != nil {
				_, err = c.DenyRequest(item.Approval.RequestID)
			}
		case "task":
			if item.Task != nil {
				if item.Task.Status == "pending_scope_expansion" {
					_, err = c.DenyExpansion(item.Task.ID)
				} else {
					_, err = c.DenyTask(item.Task.ID)
				}
			}
		}
		if err != nil {
			return approvalDoneMsg{err: err}
		}
		return approvalDoneMsg{action: "denied", id: item.ID}
	}
}

func (s *PendingScreen) fetchQueue() tea.Cmd {
	c := s.client
	return func() tea.Msg {
		resp, err := c.GetQueue()
		if err != nil {
			return tui.ErrMsg{Err: err}
		}
		return queueDataMsg{items: resp.Items, total: resp.Total}
	}
}

func (s *PendingScreen) showDetail() {
	if s.cursor >= len(s.items) {
		return
	}
	item := s.items[s.cursor]

	var title, content string
	switch item.Type {
	case "task":
		if item.Task != nil {
			title = "Task: " + item.Task.Purpose
			content = formatTaskDetail(item.Task)
		}
	case "approval":
		if item.Approval != nil {
			title = "Request: " + item.Approval.Service + "/" + item.Approval.Action
			content = formatApprovalDetail(item.Approval, item.CreatedAt)
		}
	}

	if title != "" {
		s.detail.Show(title, content)
	}
}

func (s *PendingScreen) contentWidth() int {
	w := s.width - 26
	if w < 40 {
		w = 40
	}
	return w
}

// ── Formatting helpers ──────────────────────────────────────────────────────

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
