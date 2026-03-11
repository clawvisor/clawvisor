package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/clawvisor/clawvisor/internal/tui"
)

// Sidebar renders the navigation sidebar with the active screen highlighted.
type Sidebar struct {
	Active       tui.Screen
	PendingCount int
	Width        int
	Height       int
}

var (
	sidebarStyle = lipgloss.NewStyle().
			Width(22).
			Padding(1, 1)

	activeItem = lipgloss.NewStyle().
			Foreground(tui.ColorBrand).
			Bold(true)

	inactiveItem = lipgloss.NewStyle().
			Foreground(tui.ColorDim)

	badgeStyle = lipgloss.NewStyle().
			Foreground(tui.ColorAccent).
			Bold(true)
)

func (s Sidebar) View() string {
	var b strings.Builder

	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		MarginBottom(1)
	b.WriteString(title.Render("Clawvisor"))
	b.WriteString("\n\n")

	screens := []tui.Screen{
		tui.ScreenDashboard,
		tui.ScreenTasks,
		tui.ScreenGatewayLog,
		tui.ScreenServices,
		tui.ScreenRestrictions,
		tui.ScreenAgents,
	}

	for _, screen := range screens {
		marker := "  "
		style := inactiveItem
		if screen == s.Active {
			marker = ">"
			style = activeItem
		}

		name := screen.String()
		if screen == tui.ScreenDashboard && s.PendingCount > 0 {
			badge := badgeStyle.Render(fmt.Sprintf("(%d)", s.PendingCount))
			b.WriteString(fmt.Sprintf(" %s %s %s\n", marker, style.Render(name), badge))
		} else {
			b.WriteString(fmt.Sprintf(" %s %s\n", marker, style.Render(name)))
		}
	}

	w := s.Width
	if w <= 0 {
		w = 22
	}
	h := s.Height
	if h <= 0 {
		h = 20
	}

	return sidebarStyle.
		Width(w).
		Height(h).
		Render(b.String())
}
