package screens

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/clawvisor/clawvisor/internal/tui"
)

// Help is a full-screen help overlay.
type Help struct {
	visible bool
	width   int
	height  int
}

func NewHelp() Help {
	return Help{}
}

func (h *Help) Show()       { h.visible = true }
func (h *Help) Hide()       { h.visible = false }
func (h *Help) Toggle()     { h.visible = !h.visible }
func (h Help) Visible() bool { return h.visible }

func (h Help) Update(msg tea.Msg) (Help, tea.Cmd) {
	if !h.visible {
		return h, nil
	}
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if key.Matches(msg, key.NewBinding(key.WithKeys("esc", "?", "q"))) {
			h.visible = false
		}
	case tea.WindowSizeMsg:
		h.width = msg.Width
		h.height = msg.Height
	}
	return h, nil
}

func (h Help) View() string {
	if !h.visible {
		return ""
	}

	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		Render("Keyboard Shortcuts")

	sections := []struct {
		name string
		keys [][2]string
	}{
		{
			"Global",
			[][2]string{
				{"tab / shift+tab", "Navigate sidebar"},
				{"up/down / j/k", "Navigate list items"},
				{"enter", "View details"},
				{"esc", "Close overlay / go back"},
				{"r", "Refresh current screen"},
				{"?", "Toggle this help"},
				{"q", "Quit"},
			},
		},
		{
			"Pending Approvals",
			[][2]string{
				{"a", "Approve highlighted item"},
				{"d", "Deny highlighted item"},
			},
		},
		{
			"Tasks",
			[][2]string{
				{"v", "Revoke selected task"},
			},
		},
		{
			"Restrictions / Agents",
			[][2]string{
				{"n", "Create new"},
				{"d", "Delete selected"},
			},
		},
	}

	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n\n")

	keyCol := lipgloss.NewStyle().
		Foreground(tui.ColorAccent).
		Width(20)

	sectionTitle := lipgloss.NewStyle().
		Foreground(tui.ColorWhite).
		Bold(true)

	for _, sec := range sections {
		b.WriteString(sectionTitle.Render(sec.name))
		b.WriteString("\n")
		for _, kv := range sec.keys {
			b.WriteString("  ")
			b.WriteString(keyCol.Render(kv[0]))
			b.WriteString(tui.StyleDim.Render(kv[1]))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString(tui.StyleDim.Render("[esc] Close"))

	w := h.width - 8
	if w < 40 {
		w = 40
	}
	if w > 70 {
		w = 70
	}

	return tui.StyleOverlayBorder.
		Width(w).
		Render(b.String())
}

func (h Help) SetSize(w, hh int) Help {
	h.width = w
	h.height = hh
	return h
}
