package tui

import (
	"context"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/clawvisor/clawvisor/internal/tui/client"
)

const sidebarWidth = 24

// App is the root Bubble Tea model.
type App struct {
	client       *client.Client
	screens      map[Screen]ScreenModel
	active       Screen
	width        int
	height       int
	pendingCount int
	status       string
	pollInterval time.Duration
	disconnected bool
	sseConnected bool
	sseCancel    context.CancelFunc
	sseEvents    chan client.SSEEvent
	sseErr       string // last SSE error, for status bar
	// overlay state
	showHelp bool
	debug    bool
	// version update state
	updateAvail   bool
	latestVersion string
	upgradeCmd    string
}

// NewApp creates the root TUI application.
func NewApp(c *client.Client) *App {
	return &App{
		client:       c,
		screens:      make(map[Screen]ScreenModel),
		active:       ScreenDashboard,
		pollInterval: 30 * time.Second,
		sseEvents:    make(chan client.SSEEvent, 16),
	}
}

// SetDebug enables debug indicators (SSE status) in the status bar.
func (a *App) SetDebug(on bool) { a.debug = on }

// SetScreens injects the screen implementations. Called after construction
// because screens need a reference to the client.
func (a *App) SetScreens(screens map[Screen]ScreenModel) {
	a.screens = screens
}

func (a *App) Init() tea.Cmd {
	var cmds []tea.Cmd
	// Init the active screen.
	if s, ok := a.screens[a.active]; ok {
		cmds = append(cmds, s.Init())
	}
	// Start the poll timer.
	cmds = append(cmds, PollTick(a.pollInterval))
	// Start SSE subscription.
	cmds = append(cmds, a.connectSSE())
	// Check for version updates.
	cmds = append(cmds, a.checkVersion())
	return tea.Batch(cmds...)
}

func (a *App) checkVersion() tea.Cmd {
	c := a.client
	return func() tea.Msg {
		info, err := c.GetVersion()
		if err != nil || info == nil {
			return nil
		}
		return VersionMsg{
			Latest:      info.Latest,
			UpdateAvail: info.UpdateAvail,
			UpgradeCmd:  info.UpgradeCmd,
		}
	}
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		// Forward to all screens.
		for k, s := range a.screens {
			s, cmd := s.Update(msg)
			a.screens[k] = s
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return a, tea.Batch(cmds...)

	case tea.KeyMsg:
		// Help overlay intercepts keys.
		if a.showHelp {
			if key.Matches(msg, key.NewBinding(key.WithKeys("esc", "?", "q"))) {
				a.showHelp = false
			}
			return a, nil
		}

		switch {
		case key.Matches(msg, Keys.Quit):
			return a, tea.Quit
		case key.Matches(msg, Keys.Help):
			a.showHelp = true
			return a, nil
		case key.Matches(msg, Keys.Tab):
			return a, a.nextScreen()
		case key.Matches(msg, Keys.ShiftTab):
			return a, a.prevScreen()
		}

	case TickMsg:
		// Dispatch fetch to active screen only.
		if s, ok := a.screens[a.active]; ok {
			s, cmd := s.Update(msg)
			a.screens[a.active] = s
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		cmds = append(cmds, PollTick(a.pollInterval))
		return a, tea.Batch(cmds...)

	case StatusMsg:
		a.status = string(msg)
		return a, clearStatus()

	case clearStatusMsg:
		a.status = ""
		return a, nil

	case PendingCountMsg:
		a.pendingCount = int(msg)
		return a, nil

	case ConnStateMsg:
		connected := bool(msg)
		if connected && a.disconnected {
			if a.sseConnected {
				a.pollInterval = 30 * time.Second
			} else {
				a.pollInterval = 5 * time.Second
			}
		} else if !connected && !a.disconnected {
			a.pollInterval = 15 * time.Second
		}
		a.disconnected = !connected
		return a, nil

	case SSEEventMsg:
		if !a.sseConnected {
			a.sseConnected = true
			a.sseErr = ""
			a.pollInterval = 30 * time.Second
		}
		// SSE event received — trigger a refetch on the active screen.
		if s, ok := a.screens[a.active]; ok {
			s, cmd := s.Update(TickMsg(time.Now()))
			a.screens[a.active] = s
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		// Continue listening for the next SSE event.
		cmds = append(cmds, a.waitSSE())
		return a, tea.Batch(cmds...)

	case SSEDisconnectMsg:
		a.sseConnected = false
		a.sseErr = msg.Err
		a.pollInterval = 5 * time.Second
		// Try to reconnect after a short delay.
		cmds = append(cmds, tea.Tick(3*time.Second, func(time.Time) tea.Msg {
			return sseReconnectMsg{}
		}))
		return a, tea.Batch(cmds...)

	case sseReconnectMsg:
		return a, a.connectSSE()

	case ScreenSwitchMsg:
		return a, a.switchTo(msg.Screen)

	case VersionMsg:
		a.updateAvail = msg.UpdateAvail
		a.latestVersion = msg.Latest
		a.upgradeCmd = msg.UpgradeCmd
		return a, nil
	}

	// Forward to active screen.
	if s, ok := a.screens[a.active]; ok {
		s, cmd := s.Update(msg)
		a.screens[a.active] = s
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return a, tea.Batch(cmds...)
}

func (a *App) View() string {
	if a.width == 0 {
		return "Loading..."
	}

	// Sidebar.
	sidebarView := a.renderSidebar()

	// Main content area.
	contentWidth := a.width - sidebarWidth - 1 // 1 for border
	contentHeight := a.height - 2               // status bar

	mainContent := ""
	if s, ok := a.screens[a.active]; ok {
		mainContent = s.View()
	}

	mainStyle := lipgloss.NewStyle().
		Width(contentWidth).
		Height(contentHeight)

	content := lipgloss.JoinHorizontal(
		lipgloss.Top,
		sidebarView,
		lipgloss.NewStyle().
			Foreground(ColorDim).
			Render("│"),
		mainStyle.Render(mainContent),
	)

	// Status bar.
	hints := a.activeHints()
	statusBar := a.renderStatusBar(hints)

	full := lipgloss.JoinVertical(lipgloss.Left, content, statusBar)

	// Help overlay.
	if a.showHelp {
		full = a.overlayCenter(full, a.renderHelp())
	}

	return full
}

// ── Navigation ──────────────────────────────────────────────────────────────

func (a *App) nextScreen() tea.Cmd {
	next := (int(a.active) + 1) % len(ScreenNames)
	return a.switchTo(Screen(next))
}

func (a *App) prevScreen() tea.Cmd {
	prev := (int(a.active) - 1 + len(ScreenNames)) % len(ScreenNames)
	return a.switchTo(Screen(prev))
}

func (a *App) switchTo(s Screen) tea.Cmd {
	a.active = s
	if screen, ok := a.screens[s]; ok {
		return screen.Init()
	}
	return nil
}

// ── Rendering helpers ───────────────────────────────────────────────────────

func (a *App) renderSidebar() string {
	screens := []Screen{
		ScreenDashboard,
		ScreenTasks,
		ScreenGatewayLog,
		ScreenServices,
		ScreenRestrictions,
		ScreenAgents,
	}

	sHeight := a.height - 2
	if sHeight < 5 {
		sHeight = 5
	}

	var items []string
	title := lipgloss.NewStyle().
		Foreground(ColorBrand).
		Bold(true).
		Render("Clawvisor")
	if a.updateAvail && a.latestVersion != "" {
		title += " " + lipgloss.NewStyle().
			Foreground(ColorGreen).
			Bold(true).
			Render("v"+a.latestVersion+" available")
	}
	items = append(items, title, "")

	for _, s := range screens {
		marker := "  "
		style := StyleSidebarInactive
		if s == a.active {
			marker = ">"
			style = StyleSidebarActive
		}

		name := s.String()
		line := marker + " " + style.Render(name)
		if s == ScreenDashboard && a.pendingCount > 0 {
			line += " " + StyleSidebarBadge.Render("("+itoa(a.pendingCount)+")")
		}
		items = append(items, line)
	}

	sidebar := lipgloss.JoinVertical(lipgloss.Left, items...)
	return lipgloss.NewStyle().
		Width(sidebarWidth).
		Height(sHeight).
		Padding(1, 1).
		Render(sidebar)
}

func (a *App) renderStatusBar(hints []string) string {
	globalHints := []string{
		StyleStatusKey.Render("[tab]") + StyleStatusBar.Render(" Nav"),
		StyleStatusKey.Render("[?]") + StyleStatusBar.Render(" Help"),
		StyleStatusKey.Render("[ctrl+d]") + StyleStatusBar.Render(" Quit"),
	}

	all := append(hints, globalHints...)

	sep := StyleDim.Render("  ")
	content := ""
	for i, h := range all {
		if i > 0 {
			content += sep
		}
		content += h
	}

	if a.debug {
		if a.sseConnected {
			content = StyleGreen.Render("SSE ●") + sep + content
		} else if a.sseErr != "" {
			content = StyleAmber.Render("SSE ✗ "+a.sseErr) + sep + content
		}
	}

	if a.disconnected {
		content = StyleRed.Render("Reconnecting...") + sep + content
	}

	if a.updateAvail && a.upgradeCmd != "" {
		content = StyleGreen.Render("Update: "+a.upgradeCmd) + sep + content
	}

	if a.status != "" {
		content = StyleAmber.Render(a.status) + sep + content
	}

	return StyleStatusBar.Width(a.width).Render(content)
}

func (a *App) activeHints() []string {
	if s, ok := a.screens[a.active]; ok {
		return s.ShortHelp()
	}
	return nil
}

func (a *App) renderHelp() string {
	sections := []struct {
		name string
		keys [][2]string
	}{
		{
			"Global",
			[][2]string{
				{"tab / shift+tab", "Navigate sidebar"},
				{"up/down / j/k", "Navigate list"},
				{"enter", "View details"},
				{"esc", "Close / go back"},
				{"r", "Refresh"},
				{"?", "Toggle help"},
				{"ctrl+d", "Quit"},
			},
		},
		{
			"Dashboard",
			[][2]string{
				{"a", "Approve queue item"},
				{"d", "Deny queue item"},
				{"enter", "Drill into task/request"},
				{"esc", "Back from drill-down"},
			},
		},
		{
			"Tasks",
			[][2]string{
				{"v", "Revoke"},
			},
		},
		{
			"Restrictions / Agents",
			[][2]string{
				{"n", "Create new"},
				{"d", "Delete"},
			},
		},
	}

	keyCol := lipgloss.NewStyle().Foreground(ColorAccent).Width(20)
	sectionTitle := lipgloss.NewStyle().Foreground(ColorWhite).Bold(true)
	title := lipgloss.NewStyle().Foreground(ColorBrand).Bold(true).Render("Keyboard Shortcuts")

	result := title + "\n\n"
	for _, sec := range sections {
		result += sectionTitle.Render(sec.name) + "\n"
		for _, kv := range sec.keys {
			result += "  " + keyCol.Render(kv[0]) + StyleDim.Render(kv[1]) + "\n"
		}
		result += "\n"
	}
	result += StyleDim.Render("[esc] Close")

	w := a.width - 8
	if w > 70 {
		w = 70
	}
	if w < 40 {
		w = 40
	}

	return StyleOverlayBorder.Width(w).Render(result)
}

// overlayCenter renders an overlay centered on top of the base content.
func (a *App) overlayCenter(base, overlay string) string {
	return lipgloss.Place(
		a.width, a.height,
		lipgloss.Center, lipgloss.Center,
		overlay,
		lipgloss.WithWhitespaceChars(" "),
	)
}

// ── SSE ─────────────────────────────────────────────────────────────────────

type sseReconnectMsg struct{}

// connectSSE starts an SSE subscription in a background goroutine and returns
// a tea.Cmd that waits for the first event.
func (a *App) connectSSE() tea.Cmd {
	// Cancel any existing SSE connection.
	if a.sseCancel != nil {
		a.sseCancel()
	}

	c := a.client
	ch := a.sseEvents
	ctx, cancel := context.WithCancel(context.Background())
	a.sseCancel = cancel

	return func() tea.Msg {
		ticket, err := c.GetEventTicket()
		if err != nil {
			return SSEDisconnectMsg{Err: "ticket: " + err.Error()}
		}

		// Run the SSE reader in a goroutine — it writes to ch until
		// the context is cancelled or the connection drops.
		go func() {
			sseErr := c.SubscribeSSE(ctx, ticket, ch)
			errMsg := ""
			if sseErr != nil {
				errMsg = sseErr.Error()
			}
			// Signal disconnect (non-blocking in case nobody is listening).
			select {
			case ch <- client.SSEEvent{Type: "_disconnect", ID: errMsg}:
			default:
			}
		}()

		// Wait for the first event.
		select {
		case evt := <-ch:
			if evt.Type == "_disconnect" {
				return SSEDisconnectMsg{Err: evt.ID}
			}
			return SSEEventMsg{Type: evt.Type, ID: evt.ID}
		case <-ctx.Done():
			return SSEDisconnectMsg{Err: "cancelled"}
		}
	}
}

// waitSSE returns a tea.Cmd that waits for the next SSE event from the channel.
func (a *App) waitSSE() tea.Cmd {
	ch := a.sseEvents
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok || evt.Type == "_disconnect" {
			return SSEDisconnectMsg{Err: evt.ID}
		}
		return SSEEventMsg{Type: evt.Type, ID: evt.ID}
	}
}

// ── Messages ────────────────────────────────────────────────────────────────

// PendingCountMsg updates the sidebar badge.
type PendingCountMsg int

type clearStatusMsg struct{}

func clearStatus() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg {
		return clearStatusMsg{}
	})
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
