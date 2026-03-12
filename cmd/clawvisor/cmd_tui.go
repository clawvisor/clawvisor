package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/term"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/internal/tui"
	"github.com/clawvisor/clawvisor/internal/tui/client"
	tuiconfig "github.com/clawvisor/clawvisor/internal/tui/config"
	"github.com/clawvisor/clawvisor/internal/tui/screens"
)

var (
	flagURL   string
	flagToken string
	flagDebug bool
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Launch the interactive TUI dashboard",
	RunE:  runTUI,
}

func init() {
	tuiCmd.Flags().StringVar(&flagURL, "url", "", "Server URL (overrides config)")
	tuiCmd.Flags().StringVar(&flagToken, "token", "", "Refresh token (overrides config)")
	tuiCmd.Flags().BoolVar(&flagDebug, "debug", false, "Show SSE connection status in the status bar")
}

func runTUI(cmd *cobra.Command, args []string) error {
	serverURL, token := resolveAuth()

	if serverURL == "" {
		return fmt.Errorf("no server URL configured. Run 'clawvisor setup' or pass --url")
	}

	c := client.New(serverURL, token)

	// If we already have a refresh token, try to authenticate with it.
	// Otherwise, attempt auto-login.
	if token != "" {
		if err := c.EnsureAuth(); err != nil {
			// Token is stale — fall through to auto-login.
			token = ""
		}
	}

	if token == "" {
		if err := autoLogin(c); err != nil {
			return err
		}
	}

	app := tui.NewApp(c)
	app.SetDebug(flagDebug)
	app.SetScreens(map[tui.Screen]tui.ScreenModel{
		tui.ScreenDashboard:    screens.NewDashboardScreen(c),
		tui.ScreenTasks:        screens.NewTasksScreen(c),
		tui.ScreenGatewayLog:   screens.NewActivityScreen(c),
		tui.ScreenServices:     screens.NewServicesScreen(c),
		tui.ScreenRestrictions: screens.NewRestrictionsScreen(c),
		tui.ScreenAgents:       screens.NewAgentsScreen(c),
	})

	p := tea.NewProgram(app, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// autoLogin attempts to authenticate without a pre-existing refresh token.
// For magic_link mode: reads ~/.clawvisor/.local-session and exchanges the token.
// For password mode: prompts the user for credentials.
func autoLogin(c *client.Client) error {
	cfg, err := c.GetPublicConfig()
	if err != nil {
		return fmt.Errorf("could not reach server at %s: %w", c.BaseURL(), err)
	}

	switch cfg.AuthMode {
	case "magic_link":
		sess, err := readLocalSession()
		if err != nil {
			return fmt.Errorf("no local session found. Is 'clawvisor server' running?\n  %w", err)
		}
		resp, err := c.LoginMagic(sess.MagicToken)
		if err != nil {
			return fmt.Errorf("magic token login failed (restart server for a new token): %w", err)
		}
		// Save the refresh token so future launches don't need the magic token.
		saveRefreshToken(c.BaseURL(), resp.RefreshToken)
		return nil

	case "password":
		var email string
		fmt.Print("Email: ")
		if _, err := fmt.Scanln(&email); err != nil {
			return fmt.Errorf("reading email: %w", err)
		}
		fmt.Print("Password: ")
		pwBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println() // newline after hidden input
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
		}
		password := string(pwBytes)
		resp, err := c.Login(email, password)
		if err != nil {
			return fmt.Errorf("login failed: %w", err)
		}
		saveRefreshToken(c.BaseURL(), resp.RefreshToken)
		return nil

	default:
		return fmt.Errorf("unsupported auth mode %q", cfg.AuthMode)
	}
}

// localSessionFile is the JSON structure in ~/.clawvisor/.local-session.
type localSessionFile struct {
	ServerURL  string `json:"server_url"`
	MagicToken string `json:"magic_token"`
}

// readLocalSession reads the local session file written by `clawvisor server`.
func readLocalSession() (*localSessionFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(home, ".clawvisor", ".local-session"))
	if err != nil {
		return nil, err
	}
	var sess localSessionFile
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, err
	}
	if sess.MagicToken == "" {
		return nil, fmt.Errorf("local session file has no magic token")
	}
	return &sess, nil
}

// saveRefreshToken persists the refresh token to ~/.clawvisor/config.yaml
// so subsequent TUI launches can use it directly.
func saveRefreshToken(serverURL, refreshToken string) {
	cfgPath, err := tuiconfig.DefaultPath()
	if err != nil {
		return
	}
	cfg, err := tuiconfig.Load(cfgPath)
	if err != nil {
		// Config doesn't exist yet — create a new one.
		cfg = &tuiconfig.Config{
			Profiles:      map[string]tuiconfig.Profile{},
			ActiveProfile: "default",
			PollInterval:  5,
		}
	}
	cfg.Profiles[cfg.ActiveProfile] = tuiconfig.Profile{
		ServerURL: serverURL,
		Token:     refreshToken,
	}
	_ = cfg.Save(cfgPath)
}

// resolveAuth determines server URL and token from flags, env, or config file.
func resolveAuth() (serverURL, token string) {
	serverURL = flagURL
	token = flagToken

	// Env vars as fallback.
	if serverURL == "" {
		serverURL = os.Getenv("CLAWVISOR_URL")
	}
	if token == "" {
		token = os.Getenv("CLAWVISOR_TOKEN")
	}

	// Config file as last resort.
	if serverURL == "" || token == "" {
		cfgPath, err := tuiconfig.DefaultPath()
		if err != nil {
			return
		}
		cfg, err := tuiconfig.Load(cfgPath)
		if err != nil {
			return
		}
		profile, err := cfg.Active()
		if err != nil {
			return
		}
		if serverURL == "" {
			serverURL = profile.ServerURL
		}
		if token == "" {
			token = profile.Token
		}
	}

	return
}
