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
	"github.com/clawvisor/clawvisor/internal/tui/components"
)

// ── Messages ────────────────────────────────────────────────────────────────

type tasksDataMsg struct {
	tasks []client.Task
	total int
}

type taskActionDoneMsg struct {
	action string
	err    error
}

// ── Model ───────────────────────────────────────────────────────────────────

type TasksScreen struct {
	client  *client.Client
	tasks   []client.Task
	cursor  int
	width   int
	height  int
	loading bool
	err     error
	detail  Detail
	confirm *components.Confirm
	total   int
	filter  client.TaskFilter
	showAll bool
	busy    string
}

func NewTasksScreen(c *client.Client) *TasksScreen {
	return &TasksScreen{
		client: c,
		detail: NewDetail(),
		filter: client.TaskFilter{ActiveOnly: true, Limit: 20},
	}
}

func (s *TasksScreen) Init() tea.Cmd {
	s.loading = true
	return s.fetchTasks()
}

func (s *TasksScreen) Update(msg tea.Msg) (tui.ScreenModel, tea.Cmd) {
	var cmds []tea.Cmd

	// Confirmation dialog.
	if s.confirm != nil {
		switch msg := msg.(type) {
		case components.ConfirmResult:
			s.confirm = nil
			if msg.Confirmed && msg.Tag == "revoke" {
				s.busy = "Revoking..."
				return s, s.revokeSelected()
			}
			return s, nil
		default:
			c, cmd := s.confirm.Update(msg)
			*s.confirm = c
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			return s, tea.Batch(cmds...)
		}
	}

	// Detail overlay.
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
		if newLimit := s.computePageSize(); newLimit != s.filter.Limit {
			s.filter.Limit = newLimit
			cmds = append(cmds, s.fetchTasks())
		}

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, tui.ListNavKeys.Up):
			if s.cursor > 0 {
				s.cursor--
			}
		case key.Matches(msg, tui.ListNavKeys.Down):
			if s.cursor < len(s.tasks)-1 {
				s.cursor++
			}
		case key.Matches(msg, tui.TaskKeys.Revoke):
			if s.busy != "" {
				return s, nil
			}
			if s.cursor < len(s.tasks) {
				t := s.tasks[s.cursor]
				if t.Status == "active" {
					c := components.NewConfirm(
						"Revoke Task",
						fmt.Sprintf("Revoke task \"%s\"?", t.Purpose),
						"revoke",
					)
					s.confirm = &c
				}
			}
		case key.Matches(msg, tui.TaskKeys.History):
			s.showAll = !s.showAll
			s.filter.ActiveOnly = !s.showAll
			s.filter.Offset = 0
			s.cursor = 0
			return s, s.fetchTasks()
		case key.Matches(msg, tui.ListNavKeys.Enter):
			s.showDetail()
		case key.Matches(msg, tui.Keys.Refresh):
			return s, s.fetchTasks()
		case key.Matches(msg, key.NewBinding(key.WithKeys("pgdown"))):
			if s.filter.Limit > 0 && s.filter.Offset+s.filter.Limit < s.total {
				s.filter.Offset += s.filter.Limit
				s.cursor = 0
				return s, s.fetchTasks()
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("pgup"))):
			if s.filter.Offset > 0 {
				s.filter.Offset -= s.filter.Limit
				if s.filter.Offset < 0 {
					s.filter.Offset = 0
				}
				s.cursor = 0
				return s, s.fetchTasks()
			}
		}

	case tui.TickMsg:
		return s, s.fetchTasks()

	case tasksDataMsg:
		s.loading = false
		s.err = nil
		s.tasks = msg.tasks
		s.total = msg.total
		if s.cursor >= len(s.tasks) {
			s.cursor = max(0, len(s.tasks)-1)
		}
		cmds = append(cmds, tui.ConnState(true))

	case taskActionDoneMsg:
		s.busy = ""
		if msg.err != nil {
			s.err = msg.err
		} else {
			cmds = append(cmds, func() tea.Msg {
				return tui.StatusMsg(fmt.Sprintf("Task %s", msg.action))
			})
		}
		cmds = append(cmds, s.fetchTasks())

	case tui.ErrMsg:
		s.err = msg.Err
		s.loading = false
		cmds = append(cmds, tui.ConnState(false))
	}

	return s, tea.Batch(cmds...)
}

func (s *TasksScreen) View() string {
	// Confirmation overlay.
	if s.confirm != nil {
		return s.confirm.View()
	}

	// Detail overlay.
	if s.detail.Visible() {
		return s.detail.View()
	}

	var b strings.Builder

	header := lipgloss.NewStyle().Foreground(tui.ColorWhite).Bold(true)
	title := "TASKS"
	if s.showAll {
		title = "TASKS (all)"
	}
	b.WriteString(header.Render(title))
	b.WriteString("\n")
	b.WriteString(tui.StyleDim.Render(strings.Repeat("─", min(60, s.contentWidth()))))
	b.WriteString("\n")

	if s.busy != "" {
		b.WriteString(tui.StyleAmber.Render(s.busy))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if s.err != nil {
		b.WriteString(tui.StyleRed.Render("Error: " + s.err.Error()) + "\n")
	}

	if s.loading && len(s.tasks) == 0 {
		b.WriteString(tui.StyleDim.Render("  Loading..."))
		return b.String()
	}

	if len(s.tasks) == 0 {
		if s.showAll {
			b.WriteString(tui.StyleDim.Render("  No tasks."))
		} else {
			b.WriteString(tui.StyleDim.Render("  No active tasks."))
		}
		return b.String()
	}

	// Group tasks by status.
	active, standing, recent := groupTasks(s.tasks)

	if len(active) > 0 {
		b.WriteString(tui.StyleBold.Render("ACTIVE") + "\n")
		for _, t := range active {
			sel := s.isSelected(t.idx)
			b.WriteString(s.renderTask(t.task, sel))
		}
		b.WriteString("\n")
	}

	if len(standing) > 0 {
		b.WriteString(tui.StyleBold.Render("STANDING") + "\n")
		for _, t := range standing {
			sel := s.isSelected(t.idx)
			b.WriteString(s.renderTask(t.task, sel))
		}
		b.WriteString("\n")
	}

	if len(recent) > 0 {
		b.WriteString(tui.StyleDim.Render("RECENT (completed/expired)") + "\n")
		for _, t := range recent {
			sel := s.isSelected(t.idx)
			b.WriteString(s.renderTask(t.task, sel))
		}
	}

	// Pagination info.
	if s.filter.Limit > 0 && s.total > s.filter.Limit {
		page := s.filter.Offset/s.filter.Limit + 1
		totalPages := (s.total + s.filter.Limit - 1) / s.filter.Limit
		b.WriteString("\n")
		b.WriteString(tui.StyleDim.Render(fmt.Sprintf("  Page %d/%d (%d total)  [pgup/pgdn] Navigate", page, totalPages, s.total)))
	}

	return b.String()
}

func (s *TasksScreen) ShortHelp() []string {
	help := []string{
		tui.StyleStatusKey.Render("[v]") + tui.StyleStatusBar.Render(" Revoke"),
		tui.StyleStatusKey.Render("[enter]") + tui.StyleStatusBar.Render(" Details"),
	}
	if s.showAll {
		help = append(help, tui.StyleStatusKey.Render("[h]")+tui.StyleStatusBar.Render(" Active"))
	} else {
		help = append(help, tui.StyleStatusKey.Render("[h]")+tui.StyleStatusBar.Render(" History"))
	}
	if s.filter.Limit > 0 && s.total > s.filter.Limit {
		help = append(help, tui.StyleStatusKey.Render("[pgup/pgdn]")+tui.StyleStatusBar.Render(" Page"))
	}
	return help
}

// ── Rendering ───────────────────────────────────────────────────────────────

type indexedTask struct {
	task client.Task
	idx  int
}

func groupTasks(tasks []client.Task) (active, standing, recent []indexedTask) {
	for i, t := range tasks {
		it := indexedTask{task: t, idx: i}
		switch {
		case t.Status == "active" && t.Lifetime == "standing":
			standing = append(standing, it)
		case t.Status == "active":
			active = append(active, it)
		default:
			recent = append(recent, it)
		}
	}
	return
}

func (s *TasksScreen) renderTask(t client.Task, selected bool) string {
	marker := "  "
	if selected {
		marker = tui.StyleBrand.Render("> ")
	}

	statusIcon := tui.StyleDim.Render("○")
	if t.Status == "active" {
		statusIcon = tui.StyleGreen.Render("●")
	}

	var timeInfo string
	if t.ExpiresAt != nil {
		remaining := time.Until(*t.ExpiresAt)
		if remaining > 0 {
			timeInfo = fmt.Sprintf("%d min left", int(remaining.Minutes()))
		} else {
			timeInfo = "expired"
		}
	} else if t.Lifetime == "standing" {
		timeInfo = "since " + t.CreatedAt.Format("Jan 2")
	} else {
		timeInfo = timeAgo(t.CreatedAt)
	}

	// Truncate purpose to prevent line wrapping.
	purpose := t.Purpose
	if s.width > 0 {
		// line1 overhead: marker(2) + icon(1) + space(1) + two double-space gaps(4) = 8
		purposeMax := s.width - 8 - len(t.Lifetime) - len(timeInfo)
		if purposeMax < 10 {
			purposeMax = 10
		}
		purpose = truncStr(purpose, purposeMax)
	}

	line1 := fmt.Sprintf("%s%s %s  %s  %s",
		marker,
		statusIcon,
		tui.StyleBold.Render(purpose),
		tui.StyleDim.Render(t.Lifetime),
		tui.StyleDim.Render(timeInfo),
	)

	// Actions summary.
	var actions []string
	for _, a := range t.AuthorizedActions {
		tag := "approval"
		if a.AutoExecute {
			tag = "auto"
		}
		actions = append(actions, fmt.Sprintf("%s(%s)", a.Action, tag))
	}
	actionStr := strings.Join(actions, ", ")

	agentName := t.AgentName
	if agentName == "" && len(t.AgentID) >= 8 {
		agentName = t.AgentID[:8]
	}

	requestStr := fmt.Sprintf("%d requests", t.RequestCount)

	// Truncate actions line to prevent wrapping.
	if s.width > 0 {
		// line2 overhead: 6 spaces + two double-space gaps(4) = 10
		actionMax := s.width - 10 - len(agentName) - len(requestStr)
		if actionMax < 10 {
			actionMax = 10
		}
		actionStr = truncStr(actionStr, actionMax)
	}

	line2 := fmt.Sprintf("      %s  %s  %s",
		tui.StyleDim.Render(actionStr),
		agentName,
		tui.StyleDim.Render(requestStr),
	)

	return line1 + "\n" + line2 + "\n"
}

func (s *TasksScreen) isSelected(idx int) bool {
	return idx == s.cursor
}

// ── Actions ─────────────────────────────────────────────────────────────────

func (s *TasksScreen) fetchTasks() tea.Cmd {
	c := s.client
	f := s.filter
	return func() tea.Msg {
		resp, err := c.GetTasks(f)
		if err != nil {
			return tui.ErrMsg{Err: err}
		}
		return tasksDataMsg{tasks: resp.Tasks, total: resp.Total}
	}
}

func (s *TasksScreen) revokeSelected() tea.Cmd {
	if s.cursor >= len(s.tasks) {
		return nil
	}
	t := s.tasks[s.cursor]
	c := s.client
	return func() tea.Msg {
		_, err := c.RevokeTask(t.ID)
		if err != nil {
			return taskActionDoneMsg{err: err}
		}
		return taskActionDoneMsg{action: "revoked"}
	}
}

func (s *TasksScreen) showDetail() {
	if s.cursor >= len(s.tasks) {
		return
	}
	t := s.tasks[s.cursor]
	s.detail.Show("Task: "+t.Purpose, formatTaskDetail(&t))
}

func (s *TasksScreen) contentWidth() int {
	w := s.width - 26
	if w < 40 {
		w = 40
	}
	return w
}

func (s *TasksScreen) computePageSize() int {
	if s.height <= 0 {
		return 20
	}
	size := (s.height - 8) / 2
	if size < 5 {
		size = 5
	}
	return size
}

func truncStr(s string, maxWidth int) string {
	runes := []rune(s)
	if len(runes) <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return string(runes[:maxWidth])
	}
	return string(runes[:maxWidth-3]) + "..."
}
