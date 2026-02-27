package tui

import "github.com/charmbracelet/bubbles/key"

// GlobalKeys are available on all screens.
type GlobalKeys struct {
	Quit    key.Binding
	Help    key.Binding
	Tab     key.Binding
	ShiftTab key.Binding
	Refresh key.Binding
}

var Keys = GlobalKeys{
	Quit: key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("ctrl+d", "quit"),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "help"),
	),
	Tab: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "next section"),
	),
	ShiftTab: key.NewBinding(
		key.WithKeys("shift+tab"),
		key.WithHelp("shift+tab", "prev section"),
	),
	Refresh: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "refresh"),
	),
}

// ListKeys for navigating list items.
type ListKeys struct {
	Up    key.Binding
	Down  key.Binding
	Enter key.Binding
	Esc   key.Binding
}

var ListNavKeys = ListKeys{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("up/k", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("down/j", "down"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "details"),
	),
	Esc: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "back"),
	),
}

// ApprovalKeys for the pending screen.
type ApprovalKeys struct {
	Approve key.Binding
	Deny    key.Binding
}

var PendingKeys = ApprovalKeys{
	Approve: key.NewBinding(
		key.WithKeys("a"),
		key.WithHelp("a", "approve"),
	),
	Deny: key.NewBinding(
		key.WithKeys("d"),
		key.WithHelp("d", "deny"),
	),
}

// TaskActionKeys for the tasks screen.
type TaskActionKeys struct {
	Revoke key.Binding
}

var TaskKeys = TaskActionKeys{
	Revoke: key.NewBinding(
		key.WithKeys("v"),
		key.WithHelp("v", "revoke"),
	),
}

// ManageKeys for screens with create/delete (restrictions, agents).
type ManageKeys struct {
	New    key.Binding
	Delete key.Binding
}

var ManageNavKeys = ManageKeys{
	New: key.NewBinding(
		key.WithKeys("n"),
		key.WithHelp("n", "new"),
	),
	Delete: key.NewBinding(
		key.WithKeys("d"),
		key.WithHelp("d", "delete"),
	),
}
