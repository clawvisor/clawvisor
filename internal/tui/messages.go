package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TickMsg is sent every poll interval to trigger data refresh.
type TickMsg time.Time

// PollTick returns a tea.Cmd that waits for the given duration then sends a TickMsg.
func PollTick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}

// ErrMsg carries an error from an async operation.
type ErrMsg struct{ Err error }

func (e ErrMsg) Error() string { return e.Err.Error() }

// StatusMsg is displayed briefly in the status bar.
type StatusMsg string

// ConnStateMsg indicates whether the server is reachable.
type ConnStateMsg bool

// ConnState returns a tea.Cmd that emits a ConnStateMsg.
func ConnState(connected bool) tea.Cmd {
	return func() tea.Msg { return ConnStateMsg(connected) }
}

// ScreenSwitchMsg requests switching to a different screen.
type ScreenSwitchMsg struct{ Screen Screen }

// SSEEventMsg carries an SSE event from the background subscription.
type SSEEventMsg struct {
	Type string // "queue", "tasks", "audit"
	ID   string // optional task/audit ID
}

// SSEDisconnectMsg indicates the SSE stream dropped.
type SSEDisconnectMsg struct {
	Err string // optional error detail
}

// VersionMsg carries version info fetched from the server.
type VersionMsg struct {
	Latest      string
	UpdateAvail bool
	UpgradeCmd  string
}
