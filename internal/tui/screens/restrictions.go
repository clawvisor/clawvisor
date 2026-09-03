package screens

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/clawvisor/clawvisor/internal/tui"
	"github.com/clawvisor/clawvisor/internal/tui/client"
	"github.com/clawvisor/clawvisor/internal/tui/components"
)

// ── Messages ────────────────────────────────────────────────────────────────

type restrictionsDataMsg struct {
	restrictions []client.Restriction
}

type restrictionActionDoneMsg struct {
	action string
	err    error
}

// ── Create form ─────────────────────────────────────────────────────────────

type createRestrictionForm struct {
	serviceInput textinput.Model
	actionInput  textinput.Model
	reasonInput  textinput.Model
	focus        int // 0=service, 1=action, 2=reason
}

func newCreateRestrictionForm() *createRestrictionForm {
	si := textinput.New()
	si.Placeholder = "e.g. google.gmail"
	si.Focus()

	ai := textinput.New()
	ai.Placeholder = "e.g. send_message or * for all"

	ri := textinput.New()
	ri.Placeholder = "Reason for restriction"

	return &createRestrictionForm{
		serviceInput: si,
		actionInput:  ai,
		reasonInput:  ri,
	}
}

// ── Model ───────────────────────────────────────────────────────────────────

type RestrictionsScreen struct {
	client       *client.Client
	restrictions []client.Restriction
	cursor       int
	width        int
	height       int
	loading      bool
	err          error
	confirm      *components.Confirm
	createForm   *createRestrictionForm
}

func NewRestrictionsScreen(c *client.Client) *RestrictionsScreen {
	return &RestrictionsScreen{
		client: c,
	}
}

func (s *RestrictionsScreen) Init() tea.Cmd {
	s.loading = true
	return s.fetchRestrictions()
}

func (s *RestrictionsScreen) Update(msg tea.Msg) (tui.ScreenModel, tea.Cmd) {
	var cmds []tea.Cmd

	// Create form.
	if s.createForm != nil {
		return s.updateCreateForm(msg)
	}

	// Confirm dialog.
	if s.confirm != nil {
		switch msg := msg.(type) {
		case components.ConfirmResult:
			s.confirm = nil
			if msg.Confirmed && msg.Tag == "delete-restriction" {
				return s, s.deleteSelected()
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

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		s.height = msg.Height

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, tui.ListNavKeys.Up):
			if s.cursor > 0 {
				s.cursor--
			}
		case key.Matches(msg, tui.ListNavKeys.Down):
			if s.cursor < len(s.restrictions)-1 {
				s.cursor++
			}
		case key.Matches(msg, tui.ManageNavKeys.New):
			s.createForm = newCreateRestrictionForm()
			return s, nil
		case key.Matches(msg, tui.ManageNavKeys.Delete):
			if s.cursor < len(s.restrictions) {
				r := s.restrictions[s.cursor]
				c := components.NewConfirm(
					"Delete Restriction",
					fmt.Sprintf("Delete restriction on %s/%s?", r.Service, r.Action),
					"delete-restriction",
				)
				s.confirm = &c
			}
		case key.Matches(msg, tui.Keys.Refresh):
			return s, s.fetchRestrictions()
		}

	case tui.TickMsg:
		return s, s.fetchRestrictions()

	case restrictionsDataMsg:
		s.loading = false
		s.err = nil
		s.restrictions = msg.restrictions
		if s.cursor >= len(s.restrictions) {
			s.cursor = max(0, len(s.restrictions)-1)
		}
		cmds = append(cmds, tui.ConnState(true))

	case restrictionActionDoneMsg:
		if msg.err != nil {
			s.err = msg.err
		} else {
			cmds = append(cmds, func() tea.Msg {
				return tui.StatusMsg(fmt.Sprintf("Restriction %s", msg.action))
			})
		}
		cmds = append(cmds, s.fetchRestrictions())

	case tui.ErrMsg:
		s.err = msg.Err
		s.loading = false
		cmds = append(cmds, tui.ConnState(false))
	}

	return s, tea.Batch(cmds...)
}

func (s *RestrictionsScreen) View() string {
	if s.createForm != nil {
		return s.viewCreateForm()
	}
	if s.confirm != nil {
		return s.confirm.View()
	}

	var b strings.Builder

	header := lipgloss.NewStyle().Foreground(tui.ColorWhite).Bold(true)
	b.WriteString(header.Render("RESTRICTIONS"))
	b.WriteString("\n")
	b.WriteString(tui.StyleDim.Render(strings.Repeat("─", min(60, s.contentWidth()))))
	b.WriteString("\n\n")

	if s.err != nil {
		b.WriteString(tui.StyleRed.Render("Error: " + s.err.Error()) + "\n")
	}

	if s.loading && len(s.restrictions) == 0 {
		b.WriteString(tui.StyleDim.Render("  Loading..."))
		return b.String()
	}

	if len(s.restrictions) == 0 {
		b.WriteString(tui.StyleDim.Render("  No restrictions. Press [n] to create one."))
		return b.String()
	}

	// Table header.
	colService := lipgloss.NewStyle().Width(22)
	colAction := lipgloss.NewStyle().Width(18)

	headerLine := tui.StyleTableHeader.Render(
		colService.Render("SERVICE") +
			colAction.Render("ACTION") +
			"REASON",
	)
	b.WriteString(headerLine + "\n")

	for i, r := range s.restrictions {
		selected := i == s.cursor
		marker := "  "
		if selected {
			marker = tui.StyleBrand.Render("> ")
		}

		line := marker +
			colService.Render(r.Service) +
			colAction.Render(r.Action) +
			tui.StyleDim.Render(r.Reason)

		b.WriteString(line + "\n")
	}

	return b.String()
}

func (s *RestrictionsScreen) ShortHelp() []string {
	return []string{
		tui.StyleStatusKey.Render("[n]") + tui.StyleStatusBar.Render(" New"),
		tui.StyleStatusKey.Render("[d]") + tui.StyleStatusBar.Render(" Delete"),
	}
}

// ── Create form ─────────────────────────────────────────────────────────────

func (s *RestrictionsScreen) updateCreateForm(msg tea.Msg) (tui.ScreenModel, tea.Cmd) {
	f := s.createForm

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			s.createForm = nil
			return s, nil
		case "tab", "down":
			f.focus = (f.focus + 1) % 3
			s.updateFormFocus()
			return s, nil
		case "shift+tab", "up":
			f.focus = (f.focus - 1 + 3) % 3
			s.updateFormFocus()
			return s, nil
		case "enter":
			if f.focus == 2 {
				// Submit.
				service := f.serviceInput.Value()
				action := f.actionInput.Value()
				reason := f.reasonInput.Value()
				if service == "" {
					return s, nil
				}
				if action == "" {
					action = "*"
				}
				s.createForm = nil
				return s, s.createRestriction(service, action, reason)
			}
			f.focus++
			s.updateFormFocus()
			return s, nil
		}
	}

	var cmd tea.Cmd
	switch f.focus {
	case 0:
		f.serviceInput, cmd = f.serviceInput.Update(msg)
	case 1:
		f.actionInput, cmd = f.actionInput.Update(msg)
	case 2:
		f.reasonInput, cmd = f.reasonInput.Update(msg)
	}

	return s, cmd
}

func (s *RestrictionsScreen) updateFormFocus() {
	f := s.createForm
	f.serviceInput.Blur()
	f.actionInput.Blur()
	f.reasonInput.Blur()
	switch f.focus {
	case 0:
		f.serviceInput.Focus()
	case 1:
		f.actionInput.Focus()
	case 2:
		f.reasonInput.Focus()
	}
}

func (s *RestrictionsScreen) viewCreateForm() string {
	f := s.createForm

	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		Render("New Restriction")

	var b strings.Builder
	b.WriteString(title + "\n\n")

	labels := []string{"Service:", "Action:", "Reason:"}
	inputs := []string{
		f.serviceInput.View(),
		f.actionInput.View(),
		f.reasonInput.View(),
	}

	for i := range labels {
		style := tui.StyleDim
		if i == f.focus {
			style = tui.StyleBrand
		}
		b.WriteString(style.Render(labels[i]) + "\n")
		b.WriteString("  " + inputs[i] + "\n\n")
	}

	b.WriteString(tui.StyleDim.Render("[enter] Submit  [tab] Next field  [esc] Cancel"))

	w := s.width - 8
	if w > 60 {
		w = 60
	}
	if w < 40 {
		w = 40
	}

	return tui.StyleOverlayBorder.Width(w).Render(b.String())
}

// ── Actions ─────────────────────────────────────────────────────────────────

func (s *RestrictionsScreen) fetchRestrictions() tea.Cmd {
	c := s.client
	return func() tea.Msg {
		restrictions, err := c.GetRestrictions()
		if err != nil {
			return tui.ErrMsg{Err: err}
		}
		return restrictionsDataMsg{restrictions: restrictions}
	}
}

func (s *RestrictionsScreen) createRestriction(service, action, reason string) tea.Cmd {
	c := s.client
	return func() tea.Msg {
		_, err := c.CreateRestriction(service, action, reason)
		if err != nil {
			return restrictionActionDoneMsg{err: err}
		}
		return restrictionActionDoneMsg{action: "created"}
	}
}

func (s *RestrictionsScreen) deleteSelected() tea.Cmd {
	if s.cursor >= len(s.restrictions) {
		return nil
	}
	r := s.restrictions[s.cursor]
	c := s.client
	return func() tea.Msg {
		err := c.DeleteRestriction(r.ID)
		if err != nil {
			return restrictionActionDoneMsg{err: err}
		}
		return restrictionActionDoneMsg{action: "deleted"}
	}
}

func (s *RestrictionsScreen) contentWidth() int {
	w := s.width - 26
	if w < 40 {
		w = 40
	}
	return w
}
