package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/internal/server"
	"github.com/clawvisor/clawvisor/internal/tui/client"
	"github.com/clawvisor/clawvisor/pkg/clawvisor"
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

	// Point the server at the daemon's config file.
	os.Setenv("CONFIG_FILE", filepath.Join(dataDir, "config.yaml"))

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if firstRun {
		// Phase 1 (and optionally Phase 2 for Google OAuth restart):
		// start server in background, run service setup, then hand off.
		if err := runWithServiceSetup(dataDir, logger, 1); err != nil {
			return err
		}
		fmt.Println()
		fmt.Println(green.Padding(0, 2).Render("✓ Setup complete"))
		fmt.Println(dim.Padding(0, 2).Render("  Starting daemon..."))
		fmt.Println()
	}

	// Write PID file so `daemon stop` and `daemon status` can find us.
	pidPath := filepath.Join(dataDir, ".daemon.pid")
	if err := writePIDFile(pidPath); err != nil {
		logger.Warn("could not write PID file", "err", err)
	}
	defer os.Remove(pidPath)

	// Final production run — blocks until SIGINT/SIGTERM.
	return server.Run(logger, server.RunOptions{})
}

// runWithServiceSetup starts the server with a cancellable context, waits for
// it to be healthy, runs the interactive service-setup wizard, then shuts it
// down cleanly. phase should be 1 on first call; 2 on the restart triggered
// by Google OAuth credential collection. Passing phase >= 2 disables further
// restarts, preventing infinite loops if something goes wrong.
func runWithServiceSetup(dataDir string, logger *slog.Logger, phase int) error {
	// Re-read DefaultOptions so the server picks up any config changes (e.g.
	// Google creds written between phases). Use a discarding logger so server
	// request logs don't clutter the interactive setup output.
	quietLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srvOpts, err := clawvisor.DefaultOptions(quietLogger)
	if err != nil {
		return fmt.Errorf("building server options: %w", err)
	}
	srvOpts.Quiet = true

	// Set up local auth: create admin@local, write .local-session.
	authResult, err := server.SetupLocalAuth(srvOpts, logger)
	if err != nil {
		return fmt.Errorf("local auth setup: %w", err)
	}

	// Start server in background with a caller-controlled context.
	ctx, cancel := context.WithCancel(context.Background())

	// Also cancel on SIGINT/SIGTERM so Ctrl+C during setup is clean.
	sigCtx, sigStop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer sigStop()

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- clawvisor.RunWithContext(sigCtx, srvOpts)
	}()

	// Wait for the server to be healthy before running the setup wizard.
	if err := waitForServer(authResult.ServerURL); err != nil {
		cancel()
		<-serverErrCh
		return fmt.Errorf("server failed to start: %w", err)
	}

	// Create an authenticated API client using the local magic token.
	apiClient, err := authenticateClient(authResult.ServerURL, authResult.MagicToken)
	if err != nil {
		cancel()
		<-serverErrCh
		return fmt.Errorf("authenticating API client: %w", err)
	}

	// Run the interactive service setup wizard.
	needsRestart, err := runServiceSetup(apiClient, dataDir)
	if err != nil {
		logger.Warn("service setup error", "err", err)
		// Non-fatal: user can configure services later via 'clawvisor setup'.
	}

	// Shut down the setup-phase server cleanly.
	cancel()
	if srvErr := <-serverErrCh; srvErr != nil && srvErr != context.Canceled {
		logger.Warn("server error during setup phase", "err", srvErr)
	}

	if needsRestart {
		if phase >= 2 {
			// Guard: do not restart again. The user can finish Google OAuth
			// from the dashboard. Log a warning but continue.
			logger.Warn("Google OAuth config written but server restart capped at phase 2 — complete OAuth from the dashboard")
			return nil
		}
		fmt.Println()
		fmt.Println(dim.Padding(0, 2).Render("  Restarting with updated configuration..."))
		fmt.Println()
		return runWithServiceSetup(dataDir, logger, phase+1)
	}

	return nil
}

// waitForServer polls the /health endpoint until it returns 200 or the
// deadline (10 seconds) is exceeded.
func waitForServer(serverURL string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(serverURL + "/health") //nolint:noctx
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

// SetupOptions configures the daemon setup wizard.
type SetupOptions struct {
	Pair bool // chain into device pairing after setup and print agent setup URL
}

// Setup explicitly re-runs the full daemon setup wizard (config + services)
// from the top. If a config already exists, the user is asked whether to
// overwrite it. It spins up a temporary server for service setup, so it
// works identically whether or not a daemon is already running.
func Setup(opts SetupOptions) error {
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

	// Stop any running daemon so the setup-phase server can bind the port
	// and the magic token matches the new JWT secret.
	_ = Stop()

	if err := runDaemonSetup(dataDir); err != nil {
		return err
	}

	// Point the server at the freshly written config file.
	os.Setenv("CONFIG_FILE", cfgPath)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Start a temporary server and run the service setup wizard against it,
	// just like the first-run path. This avoids depending on a running daemon
	// that may have stale config.
	if err := runWithServiceSetup(dataDir, logger, 1); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println(green.Padding(0, 2).Render("✓ Setup complete"))
	fmt.Println()

	// Offer to install and start the daemon as a background service.
	install := true
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Install and start the daemon as a background service?").
				Affirmative("Yes").
				Negative("No").
				Value(&install),
		),
	).Run(); err != nil {
		return err
	}
	if !install {
		fmt.Println(dim.Padding(0, 2).Render("  You can install later with: clawvisor install"))
		fmt.Println()
		return nil
	}

	if err := Install(); err != nil {
		return fmt.Errorf("installing daemon: %w", err)
	}
	if err := Start(); err != nil {
		return err
	}

	if opts.Pair {
		// Wait briefly for the daemon to be ready before starting the pair flow.
		time.Sleep(2 * time.Second)

		if err := Pair(); err != nil {
			return err
		}

		// Print the agent setup URL.
		printAgentSetupURL(dataDir)
	}

	return nil
}

// printAgentSetupURL reads the relay config and prints the setup URL
// that users should give to their agent.
func printAgentSetupURL(dataDir string) {
	daemonID, relayHost, err := readRelayConfig(dataDir)
	if err != nil || daemonID == "" || relayHost == "" {
		fmt.Println(dim.Padding(0, 2).Render("  Could not determine agent setup URL — relay not configured."))
		return
	}

	setupURL := fmt.Sprintf("https://%s/d/%s/skill/setup", relayHost, daemonID)

	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Connect an agent"))
	fmt.Println(dim.Padding(0, 2).Render("Copy this to your agent:"))
	fmt.Println()
	fmt.Printf("  Follow the instructions at %s\n", green.Render(setupURL))
	fmt.Println()
}
