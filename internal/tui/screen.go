package tui

import tea "github.com/charmbracelet/bubbletea"

// Screen identifies a navigation destination.
type Screen int

const (
	ScreenDashboard Screen = iota
	ScreenTasks
	ScreenGatewayLog
	ScreenServices
	ScreenRestrictions
	ScreenAgents
)

var ScreenNames = []string{
	"Dashboard",
	"Tasks",
	"Gateway Log",
	"Services",
	"Restrictions",
	"Agents",
}

func (s Screen) String() string {
	if int(s) < len(ScreenNames) {
		return ScreenNames[s]
	}
	return "Unknown"
}

// ScreenModel is the interface every screen must implement.
type ScreenModel interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (ScreenModel, tea.Cmd)
	View() string
	ShortHelp() []string // key hints for the status bar
}
