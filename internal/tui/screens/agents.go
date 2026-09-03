package screens

import (
	"fmt"
	"strings"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/clawvisor/clawvisor/internal/tui"
	"github.com/clawvisor/clawvisor/internal/tui/client"
	"github.com/clawvisor/clawvisor/internal/tui/components"
)

// ── Messages ────────────────────────────────────────────────────────────────

type agentsDataMsg struct {
	agents []client.Agent
}

type agentCreatedMsg struct {
	agent *client.Agent
	err   error
}

type agentActionDoneMsg struct {
	action string
	err    error
}

// ── Model ───────────────────────────────────────────────────────────────────

type AgentsScreen struct {
	client    *client.Client
	agents    []client.Agent
	cursor    int
	width     int
	height    int
	loading   bool
	err       error
	confirm   *components.Confirm
	nameInput *textinput.Model
	showToken *client.Agent // show token after creation
}

func NewAgentsScreen(c *client.Client) *AgentsScreen {
	return &AgentsScreen{
		client: c,
	}
}

func (s *AgentsScreen) Init() tea.Cmd {
	s.loading = true
	return s.fetchAgents()
}

func (s *AgentsScreen) Update(msg tea.Msg) (tui.ScreenModel, tea.Cmd) {
	var cmds []tea.Cmd

	// Token display.
	if s.showToken != nil {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			switch msg.String() {
			case "c":
				if s.showToken.Token != "" {
					_ = clipboard.WriteAll(s.showToken.Token)
					cmds = append(cmds, func() tea.Msg {
						return tui.StatusMsg("Token copied to clipboard")
					})
				}
				s.showToken = nil
				return s, tea.Batch(cmds...)
			default:
				s.showToken = nil
				return s, nil
			}
		}
	}

	// Name input.
	if s.nameInput != nil {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			switch msg.String() {
			case "esc":
				s.nameInput = nil
				return s, nil
			case "enter":
				name := s.nameInput.Value()
				s.nameInput = nil
				if name != "" {
					return s, s.createAgent(name)
				}
				return s, nil
			}
		}
		var cmd tea.Cmd
		*s.nameInput, cmd = s.nameInput.Update(msg)
		return s, cmd
	}

	// Confirm dialog.
	if s.confirm != nil {
		switch msg := msg.(type) {
		case components.ConfirmResult:
			s.confirm = nil
			if msg.Confirmed && msg.Tag == "delete-agent" {
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
			if s.cursor < len(s.agents)-1 {
				s.cursor++
			}
		case key.Matches(msg, tui.ManageNavKeys.New):
			ni := textinput.New()
			ni.Placeholder = "Agent name"
			ni.Focus()
			s.nameInput = &ni
			return s, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("r"))):
			// 'r' for revoke on this screen (not global refresh).
			if s.cursor < len(s.agents) {
				a := s.agents[s.cursor]
				c := components.NewConfirm(
					"Delete Agent",
					fmt.Sprintf("Delete agent \"%s\"? This revokes its token.", a.Name),
					"delete-agent",
				)
				s.confirm = &c
			}
		case key.Matches(msg, tui.Keys.Refresh):
			return s, s.fetchAgents()
		}

	case tui.TickMsg:
		return s, s.fetchAgents()

	case agentsDataMsg:
		s.loading = false
		s.err = nil
		s.agents = msg.agents
		if s.cursor >= len(s.agents) {
			s.cursor = max(0, len(s.agents)-1)
		}
		cmds = append(cmds, tui.ConnState(true))

	case agentCreatedMsg:
		if msg.err != nil {
			s.err = msg.err
		} else {
			s.showToken = msg.agent
		}
		cmds = append(cmds, s.fetchAgents())

	case agentActionDoneMsg:
		if msg.err != nil {
			s.err = msg.err
		} else {
			cmds = append(cmds, func() tea.Msg {
				return tui.StatusMsg(fmt.Sprintf("Agent %s", msg.action))
			})
		}
		cmds = append(cmds, s.fetchAgents())

	case tui.ErrMsg:
		s.err = msg.Err
		s.loading = false
		cmds = append(cmds, tui.ConnState(false))
	}

	return s, tea.Batch(cmds...)
}

func (s *AgentsScreen) View() string {
	// Token display overlay.
	if s.showToken != nil {
		return s.viewTokenDisplay()
	}

	// Name input overlay.
	if s.nameInput != nil {
		return s.viewNameInput()
	}

	// Confirm dialog.
	if s.confirm != nil {
		return s.confirm.View()
	}

	var b strings.Builder

	header := lipgloss.NewStyle().Foreground(tui.ColorWhite).Bold(true)
	b.WriteString(header.Render("AGENTS"))
	b.WriteString("\n")
	b.WriteString(tui.StyleDim.Render(strings.Repeat("─", min(60, s.contentWidth()))))
	b.WriteString("\n\n")

	if s.err != nil {
		b.WriteString(tui.StyleRed.Render("Error: " + s.err.Error()) + "\n")
	}

	if s.loading && len(s.agents) == 0 {
		b.WriteString(tui.StyleDim.Render("  Loading..."))
		return b.String()
	}

	if len(s.agents) == 0 {
		b.WriteString(tui.StyleDim.Render("  No agents. Press [n] to create one."))
		return b.String()
	}

	// Table header.
	colName := lipgloss.NewStyle().Width(16)
	colCreated := lipgloss.NewStyle().Width(18)

	headerLine := tui.StyleTableHeader.Render(
		colName.Render("NAME") +
			colCreated.Render("CREATED"),
	)
	b.WriteString(headerLine + "\n")

	for i, a := range s.agents {
		selected := i == s.cursor
		marker := "  "
		if selected {
			marker = tui.StyleBrand.Render("> ")
		}

		line := marker +
			colName.Render(a.Name) +
			colCreated.Render(a.CreatedAt.Format("Jan 2, 2006"))

		b.WriteString(line + "\n")
	}

	return b.String()
}

func (s *AgentsScreen) ShortHelp() []string {
	return []string{
		tui.StyleStatusKey.Render("[n]") + tui.StyleStatusBar.Render(" New"),
		tui.StyleStatusKey.Render("[r]") + tui.StyleStatusBar.Render(" Revoke"),
	}
}

// ── Overlays ────────────────────────────────────────────────────────────────

func (s *AgentsScreen) viewTokenDisplay() string {
	title := lipgloss.NewStyle().
		Foreground(tui.ColorGreen).
		Bold(true).
		Render("Agent Created: " + s.showToken.Name)

	token := lipgloss.NewStyle().
		Foreground(tui.ColorAccent).
		Render(s.showToken.Token)

	warning := tui.StyleDim.Render("This token is shown only once. Copy it now.")

	content := fmt.Sprintf("%s\n\n%s\n\n%s\n\n%s",
		title,
		tui.StyleDim.Render("Token:"),
		token,
		warning,
	)

	hint := tui.StyleDim.Render("[c] Copy to clipboard  [any] Close")
	content += "\n\n" + hint

	w := s.width - 8
	if w > 70 {
		w = 70
	}
	if w < 40 {
		w = 40
	}
	return tui.StyleOverlayBorder.Width(w).Render(content)
}

func (s *AgentsScreen) viewNameInput() string {
	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		Render("New Agent")

	content := fmt.Sprintf("%s\n\n%s\n%s\n\n%s",
		title,
		tui.StyleDim.Render("Name:"),
		"  "+s.nameInput.View(),
		tui.StyleDim.Render("[enter] Create  [esc] Cancel"),
	)

	w := s.width - 8
	if w > 50 {
		w = 50
	}
	if w < 40 {
		w = 40
	}
	return tui.StyleOverlayBorder.Width(w).Render(content)
}

// ── Actions ─────────────────────────────────────────────────────────────────

func (s *AgentsScreen) fetchAgents() tea.Cmd {
	c := s.client
	return func() tea.Msg {
		agents, err := c.GetAgents()
		if err != nil {
			return tui.ErrMsg{Err: err}
		}
		return agentsDataMsg{agents: agents}
	}
}

func (s *AgentsScreen) createAgent(name string) tea.Cmd {
	c := s.client
	return func() tea.Msg {
		agent, err := c.CreateAgent(name)
		if err != nil {
			return agentCreatedMsg{err: err}
		}
		return agentCreatedMsg{agent: agent}
	}
}

func (s *AgentsScreen) deleteSelected() tea.Cmd {
	if s.cursor >= len(s.agents) {
		return nil
	}
	a := s.agents[s.cursor]
	c := s.client
	return func() tea.Msg {
		err := c.DeleteAgent(a.ID)
		if err != nil {
			return agentActionDoneMsg{err: err}
		}
		return agentActionDoneMsg{action: "deleted"}
	}
}

func (s *AgentsScreen) contentWidth() int {
	w := s.width - 26
	if w < 40 {
		w = 40
	}
	return w
}
