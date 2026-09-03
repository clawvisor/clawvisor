package components

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/clawvisor/clawvisor/internal/tui"
)

// StatusBar renders key binding hints at the bottom of the screen.
type StatusBar struct {
	Width    int
	Hints    []string // e.g. ["[a] Approve", "[d] Deny"]
	Status   string   // transient status message
}

var (
	barStyle = lipgloss.NewStyle().
			Background(tui.ColorSurfaceLight).
			Padding(0, 1)

	keyStyle = lipgloss.NewStyle().
			Foreground(tui.ColorBrand).
			Background(tui.ColorSurfaceLight).
			Bold(true)

	sepStyle = lipgloss.NewStyle().
			Foreground(tui.ColorDim).
			Background(tui.ColorSurfaceLight)

	statusStyle = lipgloss.NewStyle().
			Foreground(tui.ColorAccent).
			Background(tui.ColorSurfaceLight).
			Italic(true)
)

func (s StatusBar) View() string {
	// Global keys always shown.
	global := []string{
		keyStyle.Render("[tab]") + barStyle.Render(" Nav"),
		keyStyle.Render("[?]") + barStyle.Render(" Help"),
		keyStyle.Render("[q]") + barStyle.Render(" Quit"),
	}

	all := append(s.Hints, global...)
	content := strings.Join(all, sepStyle.Render("  "))

	if s.Status != "" {
		content = statusStyle.Render(s.Status) + sepStyle.Render("  ") + content
	}

	w := s.Width
	if w <= 0 {
		w = 80
	}

	return barStyle.Width(w).Render(content)
}
