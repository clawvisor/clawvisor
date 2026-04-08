package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/internal/server"
	"github.com/clawvisor/clawvisor/internal/tui/client"
	"github.com/clawvisor/clawvisor/pkg/version"
)

// RunOptions controls daemon startup behavior.
type RunOptions struct {
	Foreground bool
}

// Run starts the daemon in the foreground. If no config.yaml exists in the
// daemon data directory, it runs the interactive setup wizard first.
func Run(opts RunOptions) error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}

	firstRun, err := ensureSetup(dataDir)
	if err != nil {
		return err
	}

	cfgPath := filepath.Join(dataDir, "config.yaml")

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if firstRun {
		fmt.Println()
		fmt.Println(dim.Padding(0, 2).Render("Tip: connect services and agents after the daemon starts:"))
		fmt.Println(dim.Padding(0, 2).Render("  clawvisor services    — connect GitHub, Gmail, Slack, etc."))
		fmt.Println(dim.Padding(0, 2).Render("  clawvisor connect-agent — connect Claude Code, Claude Desktop, etc."))
		fmt.Println()
	}

	// Write PID file so `daemon stop` and `daemon status` can find us.
	pidPath := filepath.Join(dataDir, ".daemon.pid")
	if err := writePIDFile(pidPath); err != nil {
		logger.Warn("could not write PID file", "err", err)
	}
	defer os.Remove(pidPath)

	// Final production run — blocks until SIGINT/SIGTERM.
	return server.Run(logger, server.RunOptions{ConfigPath: cfgPath})
}

// waitForServer polls the /health endpoint until it returns 200 or the
// deadline (10 seconds) is exceeded.
func waitForServer(serverURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(serverURL + "/health") //nolint:noctx
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("server at %s did not become healthy within 10s", serverURL)
}

// authenticateClient creates a TUI API client and exchanges the magic token
// for an access token.
func authenticateClient(serverURL, magicToken string) (*client.Client, error) {
	cl := client.New(serverURL, "")
	if _, err := cl.LoginMagic(magicToken); err != nil {
		return nil, fmt.Errorf("magic login: %w", err)
	}
	return cl, nil
}

// readLocalSession reads the .local-session file written by the server and
// returns the server URL and magic token.
func readLocalSession(dataDir string) (serverURL, magicToken string, err error) {
	data, err := os.ReadFile(filepath.Join(dataDir, ".local-session"))
	if err != nil {
		return "", "", err
	}
	var sess struct {
		ServerURL  string `json:"server_url"`
		MagicToken string `json:"magic_token"`
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		return "", "", err
	}
	return sess.ServerURL, sess.MagicToken, nil
}

// writePIDFile writes the current process PID to the given path.
func writePIDFile(path string) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d", os.Getpid())), 0600)
}

// readPIDFile reads a PID from the given file. Returns 0 if unreadable.
func readPIDFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		return 0
	}
	return pid
}

// isServiceInstalled returns true if a launchd plist or systemd unit exists.
func isServiceInstalled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	switch runtime.GOOS {
	case "darwin":
		_, err = os.Stat(filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"))
	case "linux":
		_, err = os.Stat(filepath.Join(home, ".config", "systemd", "user", "clawvisor.service"))
	default:
		return false
	}
	return err == nil
}

// ensureDataDir resolves and creates ~/.clawvisor if needed.
func ensureDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	dataDir := filepath.Join(home, ".clawvisor")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return "", fmt.Errorf("creating data directory: %w", err)
	}
	return dataDir, nil
}

// ensureSetup checks whether config.yaml exists in dataDir. If not, it runs
// the daemon setup wizard. Returns firstRun=true when the wizard ran.
func ensureSetup(dataDir string) (firstRun bool, err error) {
	cfgPath := filepath.Join(dataDir, "config.yaml")
	if _, err := os.Stat(cfgPath); err == nil {
		return false, nil // already configured
	}

	fmt.Println("  No config.yaml found — starting first-time setup.")
	fmt.Println()
	return true, runDaemonSetup(dataDir)
}

// Setup explicitly re-runs the core daemon config wizard (LLM, relay,
// telemetry). If a config already exists, the user is asked whether to
// overwrite it. Use `clawvisor services` and `clawvisor connect-agent` to
// connect services and agents separately.
func Setup() error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}

	cfgPath := filepath.Join(dataDir, "config.yaml")
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		// Config already exists — ask before overwriting.
		overwrite := false
		if err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("A config.yaml already exists. Overwrite and start fresh?").
					Affirmative("Yes, overwrite").
					Negative("Cancel").
					Value(&overwrite),
			),
		).Run(); err != nil {
			return err
		}
		if !overwrite {
			return nil
		}
	}

	if err := runDaemonSetup(dataDir); err != nil {
		return err
	}

	fmt.Println(green.Padding(0, 2).Render("✓ Setup complete"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("Next steps:"))
	fmt.Println(dim.Padding(0, 2).Render("  clawvisor install     — install and start the daemon"))
	fmt.Println(dim.Padding(0, 2).Render("  clawvisor services    — connect services"))
	fmt.Println(dim.Padding(0, 2).Render("  clawvisor connect-agent — connect agents"))
	fmt.Println()
	return nil
}

// relaySetupURL returns the setup URL for agents that connect via the relay.
func relaySetupURL(dataDir string) (string, bool) {
	daemonID, relayHost, err := readRelayConfig(dataDir)
	if err != nil || daemonID == "" {
		return "", false
	}

	// config.yaml doesn't store the relay URL — derive it from the build-time default.
	if relayHost == "" {
		relayHost = strings.TrimPrefix(version.RelayURL(), "wss://")
		relayHost = strings.TrimPrefix(relayHost, "ws://")
	}

	return fmt.Sprintf("https://%s/d/%s/skill/setup", relayHost, daemonID), true
}

// printAgentSetupPrompt prints a pasteable prompt that users can give to
// any other coding agent to complete Clawvisor setup.
func printAgentSetupPrompt(dataDir string) {
	setupURL, ok := relaySetupURL(dataDir)
	if !ok {
		return
	}

	fmt.Println(bold.Padding(0, 2).Render("For all other agents"))
	fmt.Println(dim.Padding(0, 2).Render("Paste the following to your agent to complete setup:"))
	fmt.Println()
	fmt.Println(green.Padding(0, 4).Render(fmt.Sprintf(
		"I'd like to set up Clawvisor as the trusted gateway for using data\n"+
			"and services. Please follow the instructions at:\n"+
			"%s", setupURL)))
	fmt.Println()
}

// printAgentSetupURL prints the raw setup URL for use in non-interactive
// contexts (e.g. after pairing).
func printAgentSetupURL(dataDir string) {
	setupURL, ok := relaySetupURL(dataDir)
	if !ok {
		fmt.Println(dim.Padding(0, 2).Render("  Could not determine agent setup URL — relay not configured."))
		return
	}

	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Agent setup URL"))
	fmt.Printf("  %s\n", green.Render(setupURL))
	fmt.Println()
}
