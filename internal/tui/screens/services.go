package screens

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/clawvisor/clawvisor/internal/browser"
	"github.com/clawvisor/clawvisor/internal/tui"
	"github.com/clawvisor/clawvisor/internal/tui/client"
	"github.com/clawvisor/clawvisor/internal/tui/components"
)

// ── Messages ────────────────────────────────────────────────────────────────

type servicesDataMsg struct {
	services []client.ServiceInfo
}

type svcActivatedMsg struct {
	err error
}

type svcDeactivatedMsg struct {
	err error
}

type oauthURLMsg struct {
	resp *client.OAuthURLResponse
	err  error
}

type oauthDoneMsg struct{}

type deviceFlowStartedMsg struct {
	resp *client.DeviceFlowStartResponse
	err  error
}

type deviceFlowPollMsg struct {
	resp *client.DeviceFlowPollResponse
	err  error
}

type pkceFlowStartedMsg struct {
	resp *client.PKCEFlowStartResponse
	err  error
}

// ── Input steps ─────────────────────────────────────────────────────────────

const (
	stepNone               = 0
	stepAlias              = 1
	stepKeyEntry           = 2
	stepOAuthConfirm       = 3
	stepOAuthWaiting       = 4
	stepDeviceFlowChoice   = 5
	stepDeviceFlowWaiting  = 6
	stepPKCEFlowChoice     = 7
	stepPKCEFlowWaiting    = 8
)

// ── Model ───────────────────────────────────────────────────────────────────

type ServicesScreen struct {
	client       *client.Client
	services     []client.ServiceInfo
	displayOrder []int // indexes into services, sorted: connected first then available
	cursor       int   // index into displayOrder
	width        int
	height       int
	loading      bool
	err          error
	detail       Detail

	// Activation flow state.
	inputStep         int
	aliasInput        *textinput.Model
	keyInput          *textinput.Model
	pendingAlias      string
	pendingOAuthURL   string
	activatingService *client.ServiceInfo

	// Deactivation.
	confirm *components.Confirm

	// OAuth completion listener.
	oauthDoneCh chan struct{}
	oauthCleanup func()

	// Device flow state.
	deviceFlowUserCode string
	deviceFlowURI      string
	deviceFlowID       string
	deviceFlowInterval int
	deviceFlowChoice   int // 0 = device flow (default), 1 = API key

	// PKCE flow state.
	pkceFlowChoice int // 0 = PKCE (default), 1 = API key
}

func NewServicesScreen(c *client.Client) *ServicesScreen {
	return &ServicesScreen{
		client: c,
		detail: NewDetail(),
	}
}

func (s *ServicesScreen) Init() tea.Cmd {
	s.loading = true
	return s.fetchServices()
}

func (s *ServicesScreen) Update(msg tea.Msg) (tui.ScreenModel, tea.Cmd) {
	var cmds []tea.Cmd

	// Detail overlay takes priority.
	if s.detail.Visible() {
		// Intercept activation/deactivation keys inside the detail view.
		if msg, isKey := msg.(tea.KeyMsg); isKey {
			if svc := s.selectedService(); svc != nil {
				switch msg.String() {
				case "a":
					if svc.RequiresActivation && (svc.Status != "activated" || !svc.CredentialFree) {
						s.detail.Hide()
						return s, s.startActivation()
					}
				case "d":
					if svc.Status == "activated" {
						s.detail.Hide()
						s.startDeactivation()
						return s, nil
					}
				}
			}
		}
		d, cmd := s.detail.Update(msg)
		s.detail = d
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if _, isKey := msg.(tea.KeyMsg); isKey {
			return s, tea.Batch(cmds...)
		}
	}

	// Confirm dialog.
	if s.confirm != nil {
		switch msg := msg.(type) {
		case components.ConfirmResult:
			s.confirm = nil
			if msg.Confirmed && msg.Tag == "deactivate-service" {
				return s, s.deactivateSelected()
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

	// Alias input overlay.
	if s.inputStep == stepAlias && s.aliasInput != nil {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			switch msg.String() {
			case "esc":
				s.resetActivation()
				return s, nil
			case "enter":
				s.pendingAlias = s.aliasInput.Value()
				s.aliasInput = nil
				return s, s.proceedAfterAlias()
			}
		}
		var cmd tea.Cmd
		*s.aliasInput, cmd = s.aliasInput.Update(msg)
		return s, cmd
	}

	// Key entry overlay.
	if s.inputStep == stepKeyEntry && s.keyInput != nil {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			switch msg.String() {
			case "esc":
				s.resetActivation()
				return s, nil
			case "enter":
				keyVal := s.keyInput.Value()
				s.keyInput = nil
				if keyVal == "" {
					s.resetActivation()
					return s, nil
				}
				return s, s.activateWithKey(keyVal)
			}
		}
		var cmd tea.Cmd
		*s.keyInput, cmd = s.keyInput.Update(msg)
		return s, cmd
	}

	// OAuth confirm overlay.
	if s.inputStep == stepOAuthConfirm {
		if msg, isKey := msg.(tea.KeyMsg); isKey {
			switch msg.String() {
			case "esc":
				s.stopOAuthListener()
				s.resetActivation()
				return s, nil
			case "enter":
				browser.Open(s.pendingOAuthURL)
				s.inputStep = stepOAuthWaiting
				return s, s.waitForOAuth()
			}
			return s, nil
		}
	}

	// OAuth waiting overlay.
	if s.inputStep == stepOAuthWaiting {
		if msg, isKey := msg.(tea.KeyMsg); isKey {
			switch msg.String() {
			case "esc":
				s.stopOAuthListener()
				s.resetActivation()
				return s, nil
			}
			_ = msg
			return s, nil
		}
	}

	// Device flow choice overlay.
	if s.inputStep == stepDeviceFlowChoice {
		if msg, isKey := msg.(tea.KeyMsg); isKey {
			switch msg.String() {
			case "esc":
				s.resetActivation()
				return s, nil
			case "up", "k":
				if s.deviceFlowChoice > 0 {
					s.deviceFlowChoice--
				}
				return s, nil
			case "down", "j":
				if s.deviceFlowChoice < 1 {
					s.deviceFlowChoice++
				}
				return s, nil
			case "enter":
				if s.deviceFlowChoice == 1 {
					// API key path.
					s.inputStep = stepKeyEntry
					ki := textinput.New()
					ki.Placeholder = "paste token here"
					ki.EchoMode = textinput.EchoPassword
					ki.Focus()
					s.keyInput = &ki
					return s, nil
				}
				// Device flow path.
				cl := s.client
				serviceID := s.activatingService.ID
				alias := s.pendingAlias
				return s, func() tea.Msg {
					resp, err := cl.DeviceFlowStart(serviceID, alias)
					return deviceFlowStartedMsg{resp: resp, err: err}
				}
			}
			return s, nil
		}
	}

	// Device flow waiting overlay.
	if s.inputStep == stepDeviceFlowWaiting {
		if msg, isKey := msg.(tea.KeyMsg); isKey {
			switch msg.String() {
			case "esc":
				s.resetActivation()
				return s, nil
			}
			return s, nil
		}
	}

	// PKCE flow choice overlay.
	if s.inputStep == stepPKCEFlowChoice {
		if msg, isKey := msg.(tea.KeyMsg); isKey {
			switch msg.String() {
			case "esc":
				s.resetActivation()
				return s, nil
			case "up", "k":
				if s.pkceFlowChoice > 0 {
					s.pkceFlowChoice--
				}
				return s, nil
			case "down", "j":
				if s.pkceFlowChoice < 1 {
					s.pkceFlowChoice++
				}
				return s, nil
			case "enter":
				if s.pkceFlowChoice == 1 {
					// API key path.
					s.inputStep = stepKeyEntry
					ki := textinput.New()
					ki.Placeholder = "paste token here"
					ki.EchoMode = textinput.EchoPassword
					ki.Focus()
					s.keyInput = &ki
					return s, nil
				}
				// PKCE flow path: start listener, get URL from server.
				port, done, cleanup := startOAuthListener()
				s.oauthDoneCh = done
				s.oauthCleanup = cleanup
				cliCallback := fmt.Sprintf("http://127.0.0.1:%d/oauth-done", port)
				cl := s.client
				serviceID := s.activatingService.ID
				alias := s.pendingAlias
				return s, func() tea.Msg {
					resp, err := cl.PKCEFlowStart(serviceID, alias, cliCallback)
					return pkceFlowStartedMsg{resp: resp, err: err}
				}
			}
			return s, nil
		}
	}

	// PKCE flow waiting overlay.
	if s.inputStep == stepPKCEFlowWaiting {
		if msg, isKey := msg.(tea.KeyMsg); isKey {
			switch msg.String() {
			case "esc":
				s.stopOAuthListener()
				s.resetActivation()
				return s, nil
			}
			return s, nil
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		s.height = msg.Height
		s.detail = s.detail.SetSize(msg.Width, msg.Height)

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, tui.ListNavKeys.Up):
			if s.cursor > 0 {
				s.cursor--
			}
		case key.Matches(msg, tui.ListNavKeys.Down):
			if s.cursor < len(s.displayOrder)-1 {
				s.cursor++
			}
		case key.Matches(msg, tui.ListNavKeys.Enter):
			s.showDetail()
		case key.Matches(msg, tui.Keys.Refresh):
			return s, s.fetchServices()
		case msg.String() == "a":
			return s, s.startActivation()
		case msg.String() == "d":
			s.startDeactivation()
		}

	case tui.TickMsg:
		return s, s.fetchServices()

	case servicesDataMsg:
		s.loading = false
		s.err = nil
		s.services = msg.services
		s.rebuildDisplayOrder()
		if s.cursor >= len(s.displayOrder) {
			s.cursor = max(0, len(s.displayOrder)-1)
		}
		cmds = append(cmds, tui.ConnState(true))

	case oauthURLMsg:
		if msg.err != nil {
			s.err = msg.err
			s.stopOAuthListener()
			s.resetActivation()
			return s, nil
		}
		if msg.resp.AlreadyAuthorized {
			s.stopOAuthListener()
			s.resetActivation()
			cmds = append(cmds, func() tea.Msg {
				return tui.StatusMsg("Already connected")
			})
			cmds = append(cmds, s.fetchServices())
			return s, tea.Batch(cmds...)
		}
		s.pendingOAuthURL = msg.resp.URL
		s.inputStep = stepOAuthConfirm

	case oauthDoneMsg:
		s.stopOAuthListener()
		s.resetActivation()
		cmds = append(cmds, func() tea.Msg {
			return tui.StatusMsg("Service connected")
		})
		cmds = append(cmds, s.fetchServices())
		return s, tea.Batch(cmds...)

	case deviceFlowStartedMsg:
		if msg.err != nil {
			s.err = msg.err
			s.resetActivation()
			return s, nil
		}
		s.deviceFlowUserCode = msg.resp.UserCode
		s.deviceFlowURI = msg.resp.VerificationURI
		s.deviceFlowID = msg.resp.FlowID
		s.deviceFlowInterval = msg.resp.Interval
		s.inputStep = stepDeviceFlowWaiting
		browser.Open(msg.resp.VerificationURI)
		return s, s.pollDeviceFlow()

	case deviceFlowPollMsg:
		if msg.err != nil {
			s.err = msg.err
			s.resetActivation()
			return s, nil
		}
		switch msg.resp.Status {
		case "complete":
			s.resetActivation()
			cmds = append(cmds, func() tea.Msg {
				return tui.StatusMsg("Service connected")
			})
			cmds = append(cmds, s.fetchServices())
			return s, tea.Batch(cmds...)
		case "pending":
			return s, s.pollDeviceFlow()
		case "slow_down":
			s.deviceFlowInterval = msg.resp.Interval
			return s, s.pollDeviceFlow()
		default: // expired, denied, error
			s.err = fmt.Errorf("device flow: %s", msg.resp.Status)
			s.resetActivation()
			return s, nil
		}

	case pkceFlowStartedMsg:
		if msg.err != nil {
			s.err = msg.err
			s.stopOAuthListener()
			s.resetActivation()
			return s, nil
		}
		s.inputStep = stepPKCEFlowWaiting
		browser.Open(msg.resp.AuthorizeURL)
		return s, s.waitForOAuth()

	case svcActivatedMsg:
		s.resetActivation()
		if msg.err != nil {
			s.err = msg.err
		} else {
			cmds = append(cmds, func() tea.Msg {
				return tui.StatusMsg("Service connected")
			})
		}
		cmds = append(cmds, s.fetchServices())
		return s, tea.Batch(cmds...)

	case svcDeactivatedMsg:
		if msg.err != nil {
			s.err = msg.err
		} else {
			cmds = append(cmds, func() tea.Msg {
				return tui.StatusMsg("Service disconnected")
			})
		}
		cmds = append(cmds, s.fetchServices())
		return s, tea.Batch(cmds...)

	case tui.ErrMsg:
		s.err = msg.Err
		s.loading = false
		cmds = append(cmds, tui.ConnState(false))
	}

	return s, tea.Batch(cmds...)
}

func (s *ServicesScreen) View() string {
	// Overlays in priority order.
	if s.detail.Visible() {
		return s.detail.View()
	}
	if s.confirm != nil {
		return s.confirm.View()
	}
	if s.inputStep == stepAlias && s.aliasInput != nil {
		return s.viewAliasInput()
	}
	if s.inputStep == stepKeyEntry && s.keyInput != nil {
		return s.viewKeyInput()
	}
	if s.inputStep == stepOAuthConfirm {
		return s.viewOAuthConfirm()
	}
	if s.inputStep == stepOAuthWaiting {
		return s.viewOAuthWaiting()
	}
	if s.inputStep == stepDeviceFlowChoice {
		return s.viewDeviceFlowChoice()
	}
	if s.inputStep == stepDeviceFlowWaiting {
		return s.viewDeviceFlowWaiting()
	}
	if s.inputStep == stepPKCEFlowChoice {
		return s.viewPKCEFlowChoice()
	}
	if s.inputStep == stepPKCEFlowWaiting {
		return s.viewPKCEFlowWaiting()
	}

	var b strings.Builder

	header := lipgloss.NewStyle().Foreground(tui.ColorWhite).Bold(true)
	b.WriteString(header.Render("SERVICES"))
	b.WriteString("\n")
	b.WriteString(tui.StyleDim.Render(strings.Repeat("─", min(60, s.contentWidth()))))
	b.WriteString("\n\n")

	if s.err != nil {
		b.WriteString(tui.StyleRed.Render("Error: " + s.err.Error()) + "\n")
	}

	if s.loading && len(s.services) == 0 {
		b.WriteString(tui.StyleDim.Render("  Loading..."))
		return b.String()
	}

	if len(s.services) == 0 {
		b.WriteString(tui.StyleDim.Render("  No services configured."))
		return b.String()
	}

	// Render in display order (connected first, then available).
	inConnected := false
	inAvailable := false
	for displayIdx, svcIdx := range s.displayOrder {
		svc := s.services[svcIdx]
		connected := svc.Status == "activated"
		if connected && !inConnected {
			inConnected = true
			b.WriteString(tui.StyleBold.Render("CONNECTED") + "\n")
		}
		if !connected && !inAvailable {
			inAvailable = true
			if inConnected {
				b.WriteString("\n")
			}
			b.WriteString(tui.StyleDim.Render("AVAILABLE (not connected)") + "\n")
		}
		sel := displayIdx == s.cursor
		b.WriteString(s.renderService(svc, sel, connected))
	}

	return b.String()
}

func (s *ServicesScreen) ShortHelp() []string {
	if svc := s.selectedService(); svc != nil {
		if svc.Status == "activated" {
			hints := []string{
				tui.StyleStatusKey.Render("[d]") + tui.StyleStatusBar.Render(" Disconnect"),
				tui.StyleStatusKey.Render("[enter]") + tui.StyleStatusBar.Render(" Details"),
			}
			if !svc.CredentialFree {
				hints = append(hints,
					tui.StyleStatusKey.Render("[a]")+tui.StyleStatusBar.Render(" Add account"),
				)
			}
			return hints
		}
		if svc.RequiresActivation {
			return []string{
				tui.StyleStatusKey.Render("[a]") + tui.StyleStatusBar.Render(" Connect"),
				tui.StyleStatusKey.Render("[enter]") + tui.StyleStatusBar.Render(" Details"),
			}
		}
	}
	return []string{
		tui.StyleStatusKey.Render("[enter]") + tui.StyleStatusBar.Render(" Details"),
	}
}

// ── Rendering ───────────────────────────────────────────────────────────────

func (s *ServicesScreen) renderService(svc client.ServiceInfo, selected, connected bool) string {
	marker := "  "
	if selected {
		marker = tui.StyleBrand.Render("> ")
	}

	icon := tui.StyleDim.Render("○")
	if connected {
		icon = tui.StyleGreen.Render("●")
	}

	name := svc.ID
	if svc.Alias != "" {
		name = svc.ID + ":" + svc.Alias
	}

	colName := lipgloss.NewStyle().Width(28)
	colDesc := lipgloss.NewStyle().Width(24)

	status := ""
	if connected {
		status = tui.StyleGreen.Render("✓ connected")
	} else if selected && svc.RequiresActivation {
		status = tui.StyleDim.Render("[a] connect")
	}

	line := marker + icon + " " +
		colName.Render(name) +
		colDesc.Render(svc.Name) +
		status

	return line + "\n"
}

// ── Overlay views ───────────────────────────────────────────────────────────

func (s *ServicesScreen) viewAliasInput() string {
	svcName := ""
	if s.activatingService != nil {
		svcName = s.activatingService.Name
	}
	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		Render("Connect " + svcName)

	content := fmt.Sprintf("%s\n\n%s\n%s\n\n%s",
		title,
		tui.StyleDim.Render("Alias (leave blank for default):"),
		"  "+s.aliasInput.View(),
		tui.StyleDim.Render("[enter] Continue  [esc] Cancel"),
	)
	return tui.StyleOverlayBorder.Width(s.overlayWidth()).Render(content)
}

func (s *ServicesScreen) viewKeyInput() string {
	svcName := ""
	if s.activatingService != nil {
		svcName = s.activatingService.Name
	}
	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		Render("Connect " + svcName)

	content := fmt.Sprintf("%s\n\n%s\n%s\n\n%s",
		title,
		tui.StyleDim.Render("API key / token:"),
		"  "+s.keyInput.View(),
		tui.StyleDim.Render("[enter] Activate  [esc] Cancel"),
	)
	return tui.StyleOverlayBorder.Width(s.overlayWidth()).Render(content)
}

func (s *ServicesScreen) viewOAuthConfirm() string {
	svcName := ""
	if s.activatingService != nil {
		svcName = s.activatingService.Name
	}
	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		Render("Connect " + svcName)

	urlLine := tui.StyleDim.Render(s.pendingOAuthURL)

	content := fmt.Sprintf("%s\n\n%s\n\n%s\n\n%s",
		title,
		"Press Enter to open browser for OAuth authorization.",
		urlLine,
		tui.StyleDim.Render("[enter] Open browser  [esc] Cancel"),
	)
	return tui.StyleOverlayBorder.Width(s.overlayWidth()).Render(content)
}

func (s *ServicesScreen) viewOAuthWaiting() string {
	svcName := ""
	if s.activatingService != nil {
		svcName = s.activatingService.Name
	}
	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		Render("Connect " + svcName)

	content := fmt.Sprintf("%s\n\n%s\n\n%s",
		title,
		tui.StyleAmber.Render("Waiting for OAuth completion in browser..."),
		tui.StyleDim.Render("[esc] Cancel"),
	)
	return tui.StyleOverlayBorder.Width(s.overlayWidth()).Render(content)
}

func (s *ServicesScreen) viewDeviceFlowChoice() string {
	svcName := ""
	if s.activatingService != nil {
		svcName = s.activatingService.Name
	}
	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		Render("Connect " + svcName)

	opts := []string{"Sign in with GitHub (browser)", "Paste a Personal Access Token"}
	var optLines strings.Builder
	for i, opt := range opts {
		cursor := "  "
		if i == s.deviceFlowChoice {
			cursor = tui.StyleBrand.Render("> ")
		}
		optLines.WriteString(cursor + opt + "\n")
	}

	content := fmt.Sprintf("%s\n\n%s\n%s",
		title,
		optLines.String(),
		tui.StyleDim.Render("[enter] Select  [esc] Cancel"),
	)
	return tui.StyleOverlayBorder.Width(s.overlayWidth()).Render(content)
}

func (s *ServicesScreen) viewDeviceFlowWaiting() string {
	svcName := ""
	if s.activatingService != nil {
		svcName = s.activatingService.Name
	}
	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		Render("Connect " + svcName)

	content := fmt.Sprintf("%s\n\n%s\n%s\n\n%s\n\n%s",
		title,
		"Enter this code in your browser:",
		lipgloss.NewStyle().Bold(true).Render("  "+s.deviceFlowUserCode),
		tui.StyleDim.Render(s.deviceFlowURI),
		tui.StyleAmber.Render("Waiting for authorization...  ")+tui.StyleDim.Render("[esc] Cancel"),
	)
	return tui.StyleOverlayBorder.Width(s.overlayWidth()).Render(content)
}

func (s *ServicesScreen) viewPKCEFlowChoice() string {
	svcName := ""
	if s.activatingService != nil {
		svcName = s.activatingService.Name
	}
	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		Render("Connect " + svcName)

	opts := []string{"Sign in with " + svcName + " (browser)", "Paste an API token"}
	var optLines strings.Builder
	for i, opt := range opts {
		cursor := "  "
		if i == s.pkceFlowChoice {
			cursor = tui.StyleBrand.Render("> ")
		}
		optLines.WriteString(cursor + opt + "\n")
	}

	content := fmt.Sprintf("%s\n\n%s\n%s",
		title,
		optLines.String(),
		tui.StyleDim.Render("[enter] Select  [esc] Cancel"),
	)
	return tui.StyleOverlayBorder.Width(s.overlayWidth()).Render(content)
}

func (s *ServicesScreen) viewPKCEFlowWaiting() string {
	svcName := ""
	if s.activatingService != nil {
		svcName = s.activatingService.Name
	}
	title := lipgloss.NewStyle().
		Foreground(tui.ColorBrand).
		Bold(true).
		Render("Connect " + svcName)

	content := fmt.Sprintf("%s\n\n%s\n\n%s",
		title,
		tui.StyleAmber.Render("Waiting for authorization in browser..."),
		tui.StyleDim.Render("[esc] Cancel"),
	)
	return tui.StyleOverlayBorder.Width(s.overlayWidth()).Render(content)
}

func (s *ServicesScreen) pollDeviceFlow() tea.Cmd {
	cl := s.client
	serviceID := s.activatingService.ID
	flowID := s.deviceFlowID
	interval := s.deviceFlowInterval
	return func() tea.Msg {
		time.Sleep(time.Duration(interval) * time.Second)
		resp, err := cl.DeviceFlowPoll(serviceID, flowID)
		return deviceFlowPollMsg{resp: resp, err: err}
	}
}

// ── Activation flow ─────────────────────────────────────────────────────────

func (s *ServicesScreen) startActivation() tea.Cmd {
	svc := s.selectedService()
	if svc == nil || !svc.RequiresActivation {
		return nil
	}

	// Credential-free services (e.g. iMessage) don't support multiple accounts.
	if svc.CredentialFree {
		if svc.Status == "activated" {
			return nil
		}
		s.activatingService = svc
		cl := s.client
		serviceID := svc.ID
		return func() tea.Msg {
			err := cl.ActivateService(serviceID)
			return svcActivatedMsg{err: err}
		}
	}

	s.activatingService = svc
	s.inputStep = stepAlias
	ni := textinput.New()
	ni.Placeholder = "default"
	ni.Focus()
	s.aliasInput = &ni
	return nil
}

func (s *ServicesScreen) proceedAfterAlias() tea.Cmd {
	svc := s.activatingService
	if svc == nil {
		s.resetActivation()
		return nil
	}

	if svc.OAuth {
		// Start local listener, then fetch OAuth URL.
		port, done, cleanup := startOAuthListener()
		s.oauthDoneCh = done
		s.oauthCleanup = cleanup
		cliCallback := fmt.Sprintf("http://localhost:%d/oauth-done", port)
		alias := s.pendingAlias
		serviceID := svc.ID
		cl := s.client
		return func() tea.Msg {
			resp, err := cl.GetOAuthURL(serviceID, alias, cliCallback)
			return oauthURLMsg{resp: resp, err: err}
		}
	}

	if svc.DeviceFlow {
		// Show choice: device flow or API key.
		s.inputStep = stepDeviceFlowChoice
		s.deviceFlowChoice = 0
		return nil
	}

	if svc.PKCEFlow {
		// Show choice: PKCE browser flow or API key.
		s.inputStep = stepPKCEFlowChoice
		s.pkceFlowChoice = 0
		return nil
	}

	// API key service — show key input.
	s.inputStep = stepKeyEntry
	ki := textinput.New()
	ki.Placeholder = "paste token here"
	ki.EchoMode = textinput.EchoPassword
	ki.Focus()
	s.keyInput = &ki
	return nil
}

func (s *ServicesScreen) activateWithKey(keyVal string) tea.Cmd {
	svc := s.activatingService
	if svc == nil {
		return nil
	}
	alias := s.pendingAlias
	serviceID := svc.ID
	cl := s.client
	return func() tea.Msg {
		err := cl.ActivateWithKey(serviceID, keyVal, alias)
		return svcActivatedMsg{err: err}
	}
}

func (s *ServicesScreen) waitForOAuth() tea.Cmd {
	ch := s.oauthDoneCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		<-ch
		return oauthDoneMsg{}
	}
}

// ── Deactivation ────────────────────────────────────────────────────────────

func (s *ServicesScreen) startDeactivation() {
	svc := s.selectedService()
	if svc == nil || svc.Status != "activated" {
		return
	}
	msg := fmt.Sprintf("Disconnect %s? This removes stored credentials.", svc.Name)
	if svc.CredentialFree {
		msg = fmt.Sprintf("Disconnect %s? Your agents will lose access.", svc.Name)
	}
	c := components.NewConfirm(
		"Disconnect Service",
		msg,
		"deactivate-service",
	)
	s.confirm = &c
}

func (s *ServicesScreen) deactivateSelected() tea.Cmd {
	svc := s.selectedService()
	if svc == nil {
		return nil
	}
	cl := s.client
	alias := svc.Alias
	return func() tea.Msg {
		err := cl.DeactivateService(svc.ID, alias)
		return svcDeactivatedMsg{err: err}
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// selectedService returns the service at the current cursor position
// (in display order), or nil if the cursor is out of range.
func (s *ServicesScreen) selectedService() *client.ServiceInfo {
	if s.cursor < 0 || s.cursor >= len(s.displayOrder) {
		return nil
	}
	return &s.services[s.displayOrder[s.cursor]]
}

// rebuildDisplayOrder computes the visual ordering: connected services first,
// then available (not connected). Called whenever s.services changes.
func (s *ServicesScreen) rebuildDisplayOrder() {
	s.displayOrder = make([]int, 0, len(s.services))
	for i, svc := range s.services {
		if svc.Status == "activated" {
			s.displayOrder = append(s.displayOrder, i)
		}
	}
	for i, svc := range s.services {
		if svc.Status != "activated" {
			s.displayOrder = append(s.displayOrder, i)
		}
	}
}

func (s *ServicesScreen) resetActivation() {
	s.inputStep = stepNone
	s.aliasInput = nil
	s.keyInput = nil
	s.pendingAlias = ""
	s.pendingOAuthURL = ""
	s.activatingService = nil
	s.deviceFlowUserCode = ""
	s.deviceFlowURI = ""
	s.deviceFlowID = ""
	s.deviceFlowInterval = 0
	s.deviceFlowChoice = 0
	s.pkceFlowChoice = 0
}

func (s *ServicesScreen) stopOAuthListener() {
	if s.oauthCleanup != nil {
		s.oauthCleanup()
		s.oauthCleanup = nil
	}
	s.oauthDoneCh = nil
}

func (s *ServicesScreen) fetchServices() tea.Cmd {
	c := s.client
	return func() tea.Msg {
		resp, err := c.GetServices()
		if err != nil {
			return tui.ErrMsg{Err: err}
		}
		return servicesDataMsg{services: resp.Services}
	}
}

func (s *ServicesScreen) showDetail() {
	svc := s.selectedService()
	if svc == nil {
		return
	}

	var b strings.Builder
	b.WriteString(tui.StyleDim.Render("ID:          ") + svc.ID + "\n")
	b.WriteString(tui.StyleDim.Render("Name:        ") + svc.Name + "\n")
	b.WriteString(tui.StyleDim.Render("Description: ") + svc.Description + "\n")

	b.WriteString(tui.StyleDim.Render("Status:      "))
	if svc.Status == "activated" {
		b.WriteString(tui.StyleGreen.Render("Connected"))
	} else {
		b.WriteString(tui.StyleAmber.Render("Not connected"))
	}
	b.WriteString("\n")

	b.WriteString(tui.StyleDim.Render("Auth:        "))
	if svc.CredentialFree {
		b.WriteString("None (local)")
	} else if svc.OAuth {
		b.WriteString("OAuth")
	} else if svc.DeviceFlow {
		b.WriteString("OAuth (device flow) or API key")
	} else if svc.PKCEFlow {
		b.WriteString("OAuth (PKCE) or API token")
	} else {
		b.WriteString("API key")
	}
	b.WriteString("\n")

	if svc.ActivatedAt != "" {
		b.WriteString(tui.StyleDim.Render("Activated:   ") + svc.ActivatedAt + "\n")
	}

	if actionNames := svc.ActionDisplayNames(); len(actionNames) > 0 {
		b.WriteString("\n" + tui.StyleBold.Render("Available Actions") + "\n")
		for _, a := range actionNames {
			b.WriteString("  " + a + "\n")
		}
	}

	// Action hint at the bottom of the detail content.
	if svc.Status != "activated" && svc.RequiresActivation {
		b.WriteString("\n" + tui.StyleAmber.Render("Press [a] to connect this service.") + "\n")
	} else if svc.Status == "activated" {
		if !svc.CredentialFree {
			b.WriteString("\n" + tui.StyleDim.Render("Press [a] to add another account.  Press [d] to disconnect.") + "\n")
		} else {
			b.WriteString("\n" + tui.StyleDim.Render("Press [d] to disconnect.") + "\n")
		}
	}

	s.detail.Show("Service: "+svc.Name, b.String())
}

func (s *ServicesScreen) contentWidth() int {
	w := s.width - 26
	if w < 40 {
		w = 40
	}
	return w
}

func (s *ServicesScreen) overlayWidth() int {
	w := s.width - 8
	if w > 60 {
		w = 60
	}
	if w < 40 {
		w = 40
	}
	return w
}

// ── OAuth local listener ────────────────────────────────────────────────────

// startOAuthListener starts a one-shot HTTP server on a random port.
// It returns the port, a channel that receives when the callback fires,
// and a cleanup function to shut down the server.
func startOAuthListener() (port int, done chan struct{}, cleanup func()) {
	done = make(chan struct{})
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		once.Do(func() { close(done) })
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		// Fallback: return a closed channel so caller doesn't block forever.
		close(done)
		return 0, done, func() {}
	}

	port = ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: mux}

	go func() { _ = srv.Serve(ln) }()

	cleanup = func() {
		once.Do(func() { close(done) })
		_ = srv.Shutdown(context.Background())
	}
	return port, done, cleanup
}
