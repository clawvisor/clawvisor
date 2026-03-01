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

// ── Dashboard messages ──────────────────────────────────────────────────────

type dashOverviewMsg struct {
	queue       []client.QueueItem
	queueTotal  int
	activeTasks []*client.Task
	activity    []client.ActivityBucket
}

type dashAuditListMsg struct {
	entries []client.AuditEntry
	total   int
}

type dashAuditDetailMsg struct {
	entry *client.AuditEntry
	err   error
}

type dashApprovalDoneMsg struct {
	action string
	err    error
}

// ── View state ──────────────────────────────────────────────────────────────

type dashView int

const (
	dashViewMain dashView = iota
	dashViewTaskAudit
	dashViewAuditDetail
)

// ── Model ───────────────────────────────────────────────────────────────────

type DashboardScreen struct {
	client *client.Client

	// Main view data.
	queue       []client.QueueItem
	queueTotal  int
	activeTasks []*client.Task
	activity    []client.ActivityBucket

	// Main view cursor spans queue items (0..len(queue)-1) then active tasks.
	cursor int

	// Drill-down state.
	view         dashView
	drillTask    *client.Task // task being drilled into
	auditEntries []client.AuditEntry
	auditCursor  int
	detail       Detail // reusable detail overlay for audit entries

	width   int
	height  int
	loading bool
	err     error
	busy    string
}

func NewDashboardScreen(c *client.Client) *DashboardScreen {
	return &DashboardScreen{
		client: c,
		detail: NewDetail(),
	}
}

func (s *DashboardScreen) Init() tea.Cmd {
	s.loading = true
	return s.fetchOverview()
}

func (s *DashboardScreen) Update(msg tea.Msg) (tui.ScreenModel, tea.Cmd) {
	var cmds []tea.Cmd

	// Audit detail overlay (deepest view).
	if s.view == dashViewAuditDetail && s.detail.Visible() {
		d, cmd := s.detail.Update(msg)
		s.detail = d
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if !s.detail.Visible() {
			s.view = dashViewTaskAudit
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
		return s.handleKey(msg)

	case tui.TickMsg:
		if s.view == dashViewMain {
			return s, s.fetchOverview()
		}

	case dashOverviewMsg:
		s.busy = ""
		s.loading = false
		s.err = nil
		s.queue = msg.queue
		s.queueTotal = msg.queueTotal
		s.activeTasks = msg.activeTasks
		s.activity = msg.activity
		s.clampMainCursor()
		cmds = append(cmds, func() tea.Msg {
			return tui.PendingCountMsg(msg.queueTotal)
		})
		cmds = append(cmds, tui.ConnState(true))

	case dashAuditListMsg:
		s.busy = ""
		s.auditEntries = msg.entries
		s.auditCursor = 0

	case dashAuditDetailMsg:
		s.busy = ""
		if msg.err != nil {
			s.err = msg.err
		} else if msg.entry != nil {
			s.view = dashViewAuditDetail
			s.detail.Show(
				"Audit: "+msg.entry.Service+"/"+msg.entry.Action,
				formatAuditDetail(msg.entry),
			)
		}

	case dashApprovalDoneMsg:
		s.busy = ""
		if msg.err != nil {
			s.err = msg.err
		} else {
			cmds = append(cmds, func() tea.Msg {
				return tui.StatusMsg(fmt.Sprintf("Item %s", msg.action))
			})
			// Return to main view after acting from drill-down.
			if s.view == dashViewTaskAudit {
				s.view = dashViewMain
				s.drillTask = nil
				s.auditEntries = nil
			}
		}
		cmds = append(cmds, s.fetchOverview())

	case tui.ErrMsg:
		s.err = msg.Err
		s.loading = false
		cmds = append(cmds, tui.ConnState(false))
	}

	return s, tea.Batch(cmds...)
}

func (s *DashboardScreen) handleKey(msg tea.KeyMsg) (tui.ScreenModel, tea.Cmd) {
	switch s.view {
	case dashViewTaskAudit:
		return s.handleTaskAuditKey(msg)
	default:
		return s.handleMainKey(msg)
	}
}

func (s *DashboardScreen) handleMainKey(msg tea.KeyMsg) (tui.ScreenModel, tea.Cmd) {
	total := s.mainItemCount()
	switch {
	case key.Matches(msg, tui.ListNavKeys.Up):
		if s.cursor > 0 {
			s.cursor--
		}
	case key.Matches(msg, tui.ListNavKeys.Down):
		if s.cursor < total-1 {
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
		if s.busy != "" {
			return s, nil
		}
		return s, s.enterSelected()
	case key.Matches(msg, tui.Keys.Refresh):
		return s, s.fetchOverview()
	}
	return s, nil
}

func (s *DashboardScreen) handleTaskAuditKey(msg tea.KeyMsg) (tui.ScreenModel, tea.Cmd) {
	switch {
	case key.Matches(msg, tui.ListNavKeys.Esc):
		s.view = dashViewMain
		s.drillTask = nil
		s.auditEntries = nil
	case key.Matches(msg, tui.ListNavKeys.Up):
		if s.auditCursor > 0 {
			s.auditCursor--
		}
	case key.Matches(msg, tui.ListNavKeys.Down):
		if s.auditCursor < len(s.auditEntries)-1 {
			s.auditCursor++
		}
	case key.Matches(msg, tui.ListNavKeys.Enter):
		if s.busy != "" {
			return s, nil
		}
		s.busy = "Loading..."
		return s, s.loadAuditDetail()
	case key.Matches(msg, tui.PendingKeys.Approve):
		if s.busy != "" {
			return s, nil
		}
		s.busy = "Approving..."
		return s, s.approveDrillTask()
	case key.Matches(msg, tui.PendingKeys.Deny):
		if s.busy != "" {
			return s, nil
		}
		s.busy = "Denying..."
		return s, s.denyDrillTask()
	}
	return s, nil
}

func (s *DashboardScreen) drillTaskIsPending() bool {
	return s.drillTask != nil &&
		(s.drillTask.Status == "pending_approval" || s.drillTask.Status == "pending_scope_expansion")
}

func (s *DashboardScreen) approveDrillTask() tea.Cmd {
	if !s.drillTaskIsPending() {
		return nil
	}
	t := s.drillTask
	c := s.client
	return func() tea.Msg {
		var err error
		if t.Status == "pending_scope_expansion" {
			_, err = c.ApproveExpansion(t.ID)
		} else {
			_, err = c.ApproveTask(t.ID)
		}
		if err != nil {
			return dashApprovalDoneMsg{err: err}
		}
		return dashApprovalDoneMsg{action: "approved"}
	}
}

func (s *DashboardScreen) denyDrillTask() tea.Cmd {
	if !s.drillTaskIsPending() {
		return nil
	}
	t := s.drillTask
	c := s.client
	return func() tea.Msg {
		var err error
		if t.Status == "pending_scope_expansion" {
			_, err = c.DenyExpansion(t.ID)
		} else {
			_, err = c.DenyTask(t.ID)
		}
		if err != nil {
			return dashApprovalDoneMsg{err: err}
		}
		return dashApprovalDoneMsg{action: "denied"}
	}
}

func (s *DashboardScreen) View() string {
	if s.view == dashViewAuditDetail && s.detail.Visible() {
		return s.detail.View()
	}
	if s.view == dashViewTaskAudit && s.drillTask != nil {
		return s.renderTaskAuditView()
	}
	return s.renderMainView()
}

func (s *DashboardScreen) ShortHelp() []string {
	switch s.view {
	case dashViewTaskAudit:
		hints := []string{
			tui.StyleStatusKey.Render("[enter]") + tui.StyleStatusBar.Render(" Details"),
			tui.StyleStatusKey.Render("[esc]") + tui.StyleStatusBar.Render(" Back"),
		}
		if s.drillTaskIsPending() {
			hints = append(hints,
				tui.StyleStatusKey.Render("[a]")+tui.StyleStatusBar.Render(" Approve"),
				tui.StyleStatusKey.Render("[d]")+tui.StyleStatusBar.Render(" Deny"),
			)
		}
		return hints
	default:
		hints := []string{
			tui.StyleStatusKey.Render("[enter]") + tui.StyleStatusBar.Render(" Drill"),
		}
		if len(s.queue) > 0 {
			hints = append(hints,
				tui.StyleStatusKey.Render("[a]")+tui.StyleStatusBar.Render(" Approve"),
				tui.StyleStatusKey.Render("[d]")+tui.StyleStatusBar.Render(" Deny"),
			)
		}
		return hints
	}
}

// ── Main view ───────────────────────────────────────────────────────────────

func (s *DashboardScreen) renderMainView() string {
	var b strings.Builder
	cw := s.contentWidth()

	header := lipgloss.NewStyle().Foreground(tui.ColorWhite).Bold(true)
	b.WriteString(header.Render("DASHBOARD"))
	b.WriteString("\n")
	b.WriteString(tui.StyleDim.Render(strings.Repeat("─", min(60, cw))))
	b.WriteString("\n")

	if s.busy != "" {
		b.WriteString(tui.StyleAmber.Render(s.busy))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if s.err != nil {
		b.WriteString(tui.StyleRed.Render("Error: "+s.err.Error()) + "\n\n")
	}

	if s.loading && len(s.queue) == 0 && len(s.activeTasks) == 0 {
		b.WriteString(tui.StyleDim.Render("  Loading..."))
		return b.String()
	}

	// Queue section.
	b.WriteString(tui.StyleBold.Render(fmt.Sprintf("QUEUE (%d)", s.queueTotal)))
	b.WriteString("\n")
	if len(s.queue) == 0 {
		b.WriteString(tui.StyleDim.Render("  No pending items."))
		b.WriteString("\n")
	} else {
		for i, item := range s.queue {
			selected := i == s.cursor
			b.WriteString(s.renderQueueItem(item, selected))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")

	// Active tasks section.
	b.WriteString(tui.StyleBold.Render(fmt.Sprintf("ACTIVE TASKS (%d)", len(s.activeTasks))))
	b.WriteString("\n")
	if len(s.activeTasks) == 0 {
		b.WriteString(tui.StyleDim.Render("  No active tasks."))
		b.WriteString("\n")
	} else {
		for i, t := range s.activeTasks {
			idx := len(s.queue) + i
			selected := idx == s.cursor
			b.WriteString(s.renderActiveTask(t, selected))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")

	// Activity sparkline.
	b.WriteString(tui.StyleBold.Render("ACTIVITY (last 60 min)"))
	b.WriteString("\n")
	spark, total := renderSparkline(s.activity)
	b.WriteString("  ")
	b.WriteString(tui.StyleBrand.Render(spark))
	b.WriteString("  ")
	b.WriteString(tui.StyleDim.Render(fmt.Sprintf("%d requests", total)))
	b.WriteString("\n")

	return b.String()
}

func (s *DashboardScreen) renderQueueItem(item client.QueueItem, selected bool) string {
	marker := "  "
	if selected {
		marker = tui.StyleBrand.Render("> ")
	}

	switch item.Type {
	case "task":
		if item.Task == nil {
			return marker + tui.StyleDim.Render("Task (no data)")
		}
		return fmt.Sprintf("%s%s %s  %s",
			marker,
			tui.StyleAmber.Render("Task:"),
			tui.StyleBold.Render(item.Task.Purpose),
			tui.StyleDim.Render(timeAgo(item.CreatedAt)),
		)
	case "approval":
		if item.Approval == nil {
			return marker + tui.StyleDim.Render("Request (no data)")
		}
		return fmt.Sprintf("%s%s %s / %s  %s",
			marker,
			tui.StyleAmber.Render("Request:"),
			item.Approval.Service,
			item.Approval.Action,
			tui.StyleDim.Render(timeAgo(item.CreatedAt)),
		)
	default:
		return marker + tui.StyleDim.Render("Unknown: "+item.Type)
	}
}

func (s *DashboardScreen) renderActiveTask(t *client.Task, selected bool) string {
	marker := "  "
	if selected {
		marker = tui.StyleBrand.Render("> ")
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
	}

	return fmt.Sprintf("%s%s %s  %s  %s  %s",
		marker,
		tui.StyleGreen.Render("●"),
		tui.StyleBold.Render(t.Purpose),
		tui.StyleDim.Render(t.Lifetime),
		tui.StyleDim.Render(timeInfo),
		tui.StyleDim.Render(fmt.Sprintf("%d requests", t.RequestCount)),
	)
}

// ── Sparkline ───────────────────────────────────────────────────────────────

func renderSparkline(buckets []client.ActivityBucket) (string, int) {
	const bars = "▁▂▃▄▅▆▇█"
	levels := []rune(bars)
	slots := make([]int, 60)
	total := 0

	now := time.Now()
	for _, b := range buckets {
		minutesAgo := int(now.Sub(b.Bucket).Minutes())
		if minutesAgo < 0 || minutesAgo >= 60 {
			continue
		}
		idx := 59 - minutesAgo // slot 0 = oldest, 59 = most recent
		slots[idx] += b.Count
		total += b.Count
	}

	maxVal := 0
	for _, v := range slots {
		if v > maxVal {
			maxVal = v
		}
	}

	if maxVal == 0 {
		return strings.Repeat(string(levels[0]), 60), total
	}

	var sb strings.Builder
	for _, v := range slots {
		idx := v * (len(levels) - 1) / maxVal
		sb.WriteRune(levels[idx])
	}
	return sb.String(), total
}

// ── Task audit drill-down view ──────────────────────────────────────────────

func (s *DashboardScreen) renderTaskAuditView() string {
	var b strings.Builder
	cw := s.contentWidth()

	header := lipgloss.NewStyle().Foreground(tui.ColorWhite).Bold(true)
	b.WriteString(header.Render("TASK: "+s.drillTask.Purpose))
	b.WriteString("\n")
	b.WriteString(tui.StyleDim.Render(strings.Repeat("─", min(60, cw))))
	b.WriteString("\n")

	if s.busy != "" {
		b.WriteString(tui.StyleAmber.Render(s.busy))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Task header info.
	b.WriteString(tui.StyleDim.Render("Status:    ") + s.drillTask.Status + "\n")
	b.WriteString(tui.StyleDim.Render("Lifetime:  ") + s.drillTask.Lifetime + "\n")
	agentName := s.drillTask.AgentName
	if agentName == "" && len(s.drillTask.AgentID) >= 8 {
		agentName = s.drillTask.AgentID[:8]
	}
	b.WriteString(tui.StyleDim.Render("Agent:     ") + agentName + "\n")
	if s.drillTask.ExpiresAt != nil {
		remaining := time.Until(*s.drillTask.ExpiresAt)
		if remaining > 0 {
			b.WriteString(tui.StyleDim.Render("Expires:   ") + fmt.Sprintf("%d min left", int(remaining.Minutes())) + "\n")
		}
	}
	if len(s.drillTask.AuthorizedActions) > 0 {
		b.WriteString("\n")
		b.WriteString(tui.StyleBold.Render("Authorized Actions") + "\n")
		for _, a := range s.drillTask.AuthorizedActions {
			auto := "per-request"
			if a.AutoExecute {
				auto = "auto"
			}
			b.WriteString(fmt.Sprintf("  %s/%s (%s)\n", a.Service, a.Action, auto))
			if a.ExpectedUse != "" {
				wrapped := wordWrap(a.ExpectedUse, cw-4) // 4 = indent
				for _, line := range wrapped {
					b.WriteString("    " + tui.StyleDim.Render(line) + "\n")
				}
			}
		}
	}

	if s.drillTask.PendingAction != nil {
		b.WriteString("\n" + tui.StyleAmber.Render("Pending Expansion") + "\n")
		b.WriteString(fmt.Sprintf("  %s/%s\n", s.drillTask.PendingAction.Service, s.drillTask.PendingAction.Action))
		if s.drillTask.PendingReason != "" {
			b.WriteString("  Reason: " + s.drillTask.PendingReason + "\n")
		}
	}
	b.WriteString("\n")

	// Audit entries table.
	b.WriteString(tui.StyleBold.Render(fmt.Sprintf("REQUESTS (%d)", len(s.auditEntries))))
	b.WriteString("\n")

	if len(s.auditEntries) == 0 {
		b.WriteString(tui.StyleDim.Render("  No requests recorded yet."))
		b.WriteString("\n")
		return b.String()
	}

	colTime := lipgloss.NewStyle().Width(12).MaxWidth(12)
	colService := lipgloss.NewStyle().Width(22).MaxWidth(22)
	colAction := lipgloss.NewStyle().Width(18).MaxWidth(18)
	colStatus := lipgloss.NewStyle().Width(10).MaxWidth(10)

	headerLine := tui.StyleTableHeader.Render(
		colTime.Render("TIME") +
			colService.Render("SERVICE") +
			colAction.Render("ACTION") +
			colStatus.Render("STATUS"),
	)
	b.WriteString(headerLine + "\n")

	for i, entry := range s.auditEntries {
		selected := i == s.auditCursor
		marker := "  "
		if selected {
			marker = tui.StyleBrand.Render("> ")
		}

		timeStr := entry.Timestamp.Format("3:04 PM")
		statusIcon := outcomeIcon(entry.Outcome)

		line := marker +
			colTime.Render(timeStr) +
			colService.Render(entry.Service) +
			colAction.Render(entry.Action) +
			colStatus.Render(statusIcon)
		b.WriteString(line + "\n")
	}

	return b.String()
}

func outcomeIcon(outcome string) string {
	switch outcome {
	case "executed":
		return tui.StyleGreen.Render("✓ exec")
	case "denied":
		return tui.StyleRed.Render("✗ deny")
	case "pending":
		return tui.StyleAmber.Render("⏳ pend")
	case "timeout":
		return tui.StyleDim.Render("⏱ time")
	case "error":
		return tui.StyleRed.Render("! err")
	default:
		return tui.StyleDim.Render(outcome)
	}
}

// ── Data fetching ───────────────────────────────────────────────────────────

func (s *DashboardScreen) fetchOverview() tea.Cmd {
	c := s.client
	return func() tea.Msg {
		resp, err := c.GetOverview()
		if err != nil {
			return tui.ErrMsg{Err: err}
		}
		// Convert []*Task to match our local field type.
		return dashOverviewMsg{
			queue:       resp.Queue,
			queueTotal:  resp.QueueTotal,
			activeTasks: resp.ActiveTasks,
			activity:    resp.Activity,
		}
	}
}

func (s *DashboardScreen) fetchTaskAudit(taskID string) tea.Cmd {
	c := s.client
	return func() tea.Msg {
		resp, err := c.GetAudit(client.AuditFilter{TaskID: taskID, Limit: 50})
		if err != nil {
			return tui.ErrMsg{Err: err}
		}
		return dashAuditListMsg{entries: resp.Entries, total: resp.Total}
	}
}

func (s *DashboardScreen) loadAuditDetail() tea.Cmd {
	if s.auditCursor >= len(s.auditEntries) {
		return nil
	}
	entry := s.auditEntries[s.auditCursor]
	c := s.client
	return func() tea.Msg {
		full, err := c.GetAuditEntry(entry.ID)
		if err != nil {
			return dashAuditDetailMsg{err: err}
		}
		return dashAuditDetailMsg{entry: full}
	}
}

// ── Actions ─────────────────────────────────────────────────────────────────

func (s *DashboardScreen) approveSelected() tea.Cmd {
	if s.cursor >= len(s.queue) {
		return nil
	}
	item := s.queue[s.cursor]
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
			return dashApprovalDoneMsg{err: err}
		}
		return dashApprovalDoneMsg{action: "approved"}
	}
}

func (s *DashboardScreen) denySelected() tea.Cmd {
	if s.cursor >= len(s.queue) {
		return nil
	}
	item := s.queue[s.cursor]
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
			return dashApprovalDoneMsg{err: err}
		}
		return dashApprovalDoneMsg{action: "denied"}
	}
}

func (s *DashboardScreen) enterSelected() tea.Cmd {
	total := s.mainItemCount()
	if s.cursor >= total {
		return nil
	}

	// Queue item — show detail overlay.
	if s.cursor < len(s.queue) {
		item := s.queue[s.cursor]
		switch item.Type {
		case "task":
			if item.Task != nil {
				s.busy = "Loading..."
				s.drillTask = taskPtrFromQueueTask(item.Task)
				s.view = dashViewTaskAudit
				return s.fetchTaskAudit(item.Task.ID)
			}
		case "approval":
			if item.Approval != nil {
				s.detail.Show(
					"Request: "+item.Approval.Service+"/"+item.Approval.Action,
					formatApprovalDetail(item.Approval, item.CreatedAt),
				)
				s.view = dashViewAuditDetail
			}
		}
		return nil
	}

	// Active task — drill into audit list.
	taskIdx := s.cursor - len(s.queue)
	if taskIdx < len(s.activeTasks) {
		t := s.activeTasks[taskIdx]
		s.busy = "Loading..."
		s.drillTask = t
		s.view = dashViewTaskAudit
		return s.fetchTaskAudit(t.ID)
	}

	return nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func (s *DashboardScreen) mainItemCount() int {
	return len(s.queue) + len(s.activeTasks)
}

func (s *DashboardScreen) clampMainCursor() {
	total := s.mainItemCount()
	if s.cursor >= total {
		s.cursor = max(0, total-1)
	}
}

func (s *DashboardScreen) contentWidth() int {
	w := s.width - 26
	if w < 40 {
		w = 40
	}
	return w
}

// taskPtrFromQueueTask converts a *client.Task embedded in a QueueItem to a
// *client.Task suitable for drillTask. QueueItem.Task is already a pointer.
func taskPtrFromQueueTask(t *client.Task) *client.Task {
	return t
}

// wordWrap breaks text into lines that fit within maxWidth, splitting at word
// boundaries. Words longer than maxWidth are placed on their own line unsplit.
func wordWrap(text string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{text}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > maxWidth {
			lines = append(lines, line)
			line = w
		} else {
			line += " " + w
		}
	}
	lines = append(lines, line)
	return lines
}
