package tui

import (
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
	// overlay state
	showHelp bool
}

// NewApp creates the root TUI application.
func NewApp(c *client.Client) *App {
	return &App{
		client:       c,
		screens:      make(map[Screen]ScreenModel),
		active:       ScreenDashboard,
		pollInterval: 5 * time.Second,
	}
}

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
	return tea.Batch(cmds...)
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
			a.pollInterval = 5 * time.Second
		} else if !connected && !a.disconnected {
			a.pollInterval = 15 * time.Second
		}
		a.disconnected = !connected
		return a, nil

	case ScreenSwitchMsg:
		return a, a.switchTo(msg.Screen)
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
		ScreenPending,
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
		if s == ScreenPending && a.pendingCount > 0 {
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

	if a.disconnected {
		content = StyleRed.Render("Reconnecting...") + sep + content
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
			"Pending",
			[][2]string{
				{"a", "Approve"},
				{"d", "Deny"},
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
