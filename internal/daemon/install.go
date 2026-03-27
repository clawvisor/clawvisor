package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/charmbracelet/huh"
)

const launchdLabel = "com.clawvisor.daemon"

const launchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.clawvisor.daemon</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.Binary}}</string>
        <string>start</string>
        <string>--foreground</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>{{.LogDir}}/daemon.out.log</string>
    <key>StandardErrorPath</key>
    <string>{{.LogDir}}/daemon.err.log</string>
    <key>WorkingDirectory</key>
    <string>{{.DataDir}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin</string>
        <key>CONFIG_FILE</key>
        <string>{{.DataDir}}/config.yaml</string>
    </dict>
</dict>
</plist>
`

const systemdUnit = `[Unit]
Description=Clawvisor Daemon
After=network-online.target

[Service]
ExecStart={{.Binary}} start --foreground
Restart=always
RestartSec=5
Environment=CONFIG_FILE={{.DataDir}}/config.yaml

[Install]
WantedBy=default.target
`

type installData struct {
	Binary  string
	LogDir  string
	DataDir string
}

// isGoRunBinary reports whether the given path looks like a temporary binary
// built by `go run` (lives under the Go build cache or os.TempDir()).
func isGoRunBinary(path string) bool {
	return strings.Contains(path, "go-build") || strings.Contains(path, "/exe/")
}

// Install writes a service definition so the daemon starts at login.
// If no config.yaml exists in ~/.clawvisor, the setup wizard runs first.
func Install() error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}
	firstRun, err := ensureSetup(dataDir)
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}

	if firstRun {
		// Stop any running daemon so the setup-phase server can bind the
		// port and the magic token matches the new in-memory store.
		_ = Stop()

		cfgPath := filepath.Join(dataDir, "config.yaml")
		logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
		if err := runWithServiceSetup(dataDir, cfgPath, logger, 1); err != nil {
			return fmt.Errorf("service setup: %w", err)
		}
		fmt.Println()
		fmt.Println(green.Padding(0, 2).Render("✓ Setup complete"))
		fmt.Println()
	}

	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return fmt.Errorf("resolving symlinks: %w", err)
	}

	// go run builds a temporary binary that won't exist after the process
	// exits, so installing it as a service would always fail to launch.
	if isGoRunBinary(binary) {
		return fmt.Errorf("cannot install a service from `go run` — build a binary first:\n  go build -o clawvisor ./cmd/clawvisor && ./clawvisor install")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	logDir := filepath.Join(home, ".clawvisor", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}

	data := installData{Binary: binary, LogDir: logDir, DataDir: dataDir}

	switch runtime.GOOS {
	case "darwin":
		if err := installLaunchd(home, data); err != nil {
			return err
		}
	case "linux":
		if err := installSystemd(home, data); err != nil {
			return err
		}
	default:
		return fmt.Errorf("auto-install is supported on macOS and Linux; start the daemon manually with `clawvisor start`")
	}

	// Start the daemon and print the agent setup URL.
	if err := Start(); err != nil {
		return err
	}
	printAgentSetupInstructions(dataDir)
	promptDashboardOpen(dataDir)
	return nil
}

func installLaunchd(home string, data installData) error {
	plistDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDir, 0755); err != nil {
		return fmt.Errorf("creating LaunchAgents directory: %w", err)
	}

	plistPath := filepath.Join(plistDir, launchdLabel+".plist")
	f, err := os.Create(plistPath)
	if err != nil {
		return fmt.Errorf("creating plist file: %w", err)
	}
	defer f.Close()

	tmpl, err := template.New("plist").Parse(launchdPlist)
	if err != nil {
		return fmt.Errorf("parsing plist template: %w", err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("writing plist: %w", err)
	}

	fmt.Printf("  Installed launch agent: %s\n", plistPath)
	fmt.Println("  To start now: clawvisor start")
	fmt.Println("  To stop:      clawvisor stop")
	return nil
}

// updatePlistBinary rewrites ProgramArguments[0] in the plist to match the
// currently running executable. This ensures `daemon start` after an upgrade
// always launches the freshly built binary.
func updatePlistBinary(plistPath string) error {
	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable: %w", err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return fmt.Errorf("resolving symlinks: %w", err)
	}
	if isGoRunBinary(binary) {
		return nil // don't persist a go-run temp path
	}

	// Read current value to avoid unnecessary writes.
	out, err := exec.Command("/usr/libexec/PlistBuddy", "-c", "Print :ProgramArguments:0", plistPath).Output()
	if err != nil {
		return fmt.Errorf("reading plist: %w", err)
	}
	if strings.TrimSpace(string(out)) == binary {
		return nil // already correct
	}

	if err := exec.Command("/usr/libexec/PlistBuddy", "-c",
		fmt.Sprintf("Set :ProgramArguments:0 %s", binary), plistPath).Run(); err != nil {
		return fmt.Errorf("updating plist: %w", err)
	}
	return nil
}

func installSystemd(home string, data installData) error {
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0755); err != nil {
		return fmt.Errorf("creating systemd user directory: %w", err)
	}

	unitPath := filepath.Join(unitDir, "clawvisor.service")
	f, err := os.Create(unitPath)
	if err != nil {
		return fmt.Errorf("creating unit file: %w", err)
	}
	defer f.Close()

	tmpl, err := template.New("unit").Parse(systemdUnit)
	if err != nil {
		return fmt.Errorf("parsing unit template: %w", err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("writing unit file: %w", err)
	}

	// Reload so systemd sees the new unit.
	exec.Command("systemctl", "--user", "daemon-reload").Run()

	fmt.Printf("  Installed systemd user service: %s\n", unitPath)
	fmt.Println("  To start now: clawvisor start")
	fmt.Println("  To stop:      clawvisor stop")
	return nil
}

// promptDashboardOpen asks the user whether they want to open the dashboard
// now, and either opens it or prints the command to do so later.
func promptDashboardOpen(dataDir string) {
	if nonInteractive() {
		fmt.Println(dim.Padding(0, 2).Render("Run `clawvisor dashboard` to open the dashboard."))
		return
	}

	openNow := true
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Open the dashboard now?").
				Affirmative("Yes").
				Negative("No").
				Value(&openNow),
		),
	).Run()
	if err != nil {
		// User aborted or input error — just print the hint.
		fmt.Println(dim.Padding(0, 2).Render("Run `clawvisor dashboard` to open the dashboard."))
		return
	}

	if !openNow {
		fmt.Println(dim.Padding(0, 2).Render("Run `clawvisor dashboard` to open the dashboard."))
		return
	}

	// The daemon was just started via the service manager — wait for it to
	// actually be healthy before requesting a magic token. We can't just
	// check for .local-session because the setup-phase server already wrote
	// one that's now stale.
	serverURL, _, _ := readLocalSession(dataDir)
	if serverURL == "" {
		serverURL = "http://127.0.0.1:25297"
	}
	fmt.Println(dim.Padding(0, 2).Render("Waiting for daemon to start..."))
	if err := waitForServer(serverURL); err != nil {
		fmt.Println(dim.Padding(0, 2).Render("Daemon hasn't started yet. Run `clawvisor dashboard` once it's ready."))
		return
	}

	if err := Dashboard(false); err != nil {
		fmt.Println(dim.Padding(0, 2).Render("Could not open dashboard: " + err.Error()))
		fmt.Println(dim.Padding(0, 2).Render("Run `clawvisor dashboard` to try again."))
	}
}

// Uninstall removes the service definition.
func Uninstall() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		// Stop first if running.
		exec.Command("launchctl", "unload", plistPath).Run()
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing plist: %w", err)
		}
		fmt.Println("  Uninstalled launch agent.")
	case "linux":
		exec.Command("systemctl", "--user", "stop", "clawvisor.service").Run()
		exec.Command("systemctl", "--user", "disable", "clawvisor.service").Run()
		unitPath := filepath.Join(home, ".config", "systemd", "user", "clawvisor.service")
		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing unit file: %w", err)
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		fmt.Println("  Uninstalled systemd user service.")
	default:
		return fmt.Errorf("auto-uninstall is supported on macOS and Linux")
	}
	return nil
}

// Start activates the installed daemon service.
// On macOS it also updates the plist binary path to match the current
// executable so that upgrades take effect without a separate install step.
func Start() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		if _, err := os.Stat(plistPath); os.IsNotExist(err) {
			return fmt.Errorf("daemon not installed; run `clawvisor install` first")
		}

		// Update the plist binary path to the current executable so that
		// upgrades (build + restart) pick up the new binary automatically.
		if err := updatePlistBinary(plistPath); err != nil {
			fmt.Printf("  Warning: could not update plist binary path: %v\n", err)
		}

		// Unload first in case an old/stale plist was already loaded.
		exec.Command("launchctl", "unload", plistPath).Run() //nolint:errcheck
		out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl load: %s", string(out))
		}
		fmt.Println("  Daemon started.")
	case "linux":
		out, err := exec.Command("systemctl", "--user", "start", "clawvisor.service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl start: %s", string(out))
		}
		fmt.Println("  Daemon started.")
	default:
		return fmt.Errorf("use `clawvisor start` to start the daemon on this platform")
	}
	return nil
}

// Stop deactivates the running daemon service. It tries launchd/systemd first,
// then falls back to signalling the PID from .daemon.pid if the server is still
// responding to health checks.
func Stop() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	// Try the service manager first (best-effort, may not be installed).
	switch runtime.GOOS {
	case "darwin":
		plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		exec.Command("launchctl", "unload", plistPath).Run() //nolint:errcheck
	case "linux":
		exec.Command("systemctl", "--user", "stop", "clawvisor.service").Run() //nolint:errcheck
	}

	// Check if the daemon is still alive and signal it directly if so.
	dataDir := filepath.Join(home, ".clawvisor")
	pid := readPIDFile(filepath.Join(dataDir, ".daemon.pid"))
	if pid > 0 {
		proc, err := os.FindProcess(pid)
		if err == nil {
			// Check if process is actually alive (signal 0).
			if proc.Signal(syscall.Signal(0)) == nil {
				if err := proc.Signal(syscall.SIGTERM); err != nil {
					return fmt.Errorf("signalling daemon (PID %d): %w", pid, err)
				}
				// Wait briefly for it to exit.
				for i := 0; i < 30; i++ {
					time.Sleep(100 * time.Millisecond)
					if proc.Signal(syscall.Signal(0)) != nil {
						break
					}
				}
			}
		}
		os.Remove(filepath.Join(dataDir, ".daemon.pid"))
	}

	fmt.Println("  Daemon stopped.")
	return nil
}

// printAgentSetupInstructions prints instructions for connecting agents.
// If the Claude Code slash command is installed, it mentions /clawvisor-setup.
// Otherwise, if the relay is configured, it prints the setup URL.
func printAgentSetupInstructions(dataDir string) {
	fmt.Println()
	fmt.Println(green.Padding(0, 2).Render("✓ Installation complete"))
	fmt.Println()

	home, _ := os.UserHomeDir()

	// Check if the Claude Code command was installed.
	claudeCmdPath := filepath.Join(home, ".claude", "commands", "clawvisor-setup.md")
	if _, err := os.Stat(claudeCmdPath); err == nil {
		fmt.Println(bold.Padding(0, 2).Render("Connect Claude Code"))
		fmt.Println(dim.Padding(0, 2).Render("  Run /clawvisor-setup in any Claude Code project."))
		fmt.Println()
	}

	// Offer to configure Claude Desktop MCP if detected. We wait for the
	// daemon to be healthy first so that mcp-remote can register an OAuth
	// client when Claude Desktop restarts.
	for _, a := range appBundleAgents {
		if a.Binary != "claude-desktop" {
			continue
		}
		for _, p := range a.Paths {
			if _, err := os.Stat(p); err == nil {
				serverURL, _, _ := readLocalSession(dataDir)
				if serverURL == "" {
					serverURL = "http://127.0.0.1:25297"
				}
				if err := waitForServer(serverURL); err != nil {
					// Daemon not ready — fall back to manual instructions.
					printClaudeDesktopManualInstructions()
				} else {
					offerClaudeDesktopSetup()
				}
				break
			}
		}
		break
	}

	daemonID, relayHost, err := readRelayConfig(dataDir)
	if err != nil || daemonID == "" || relayHost == "" {
		return
	}

	setupURL := fmt.Sprintf("https://%s/d/%s/skill/setup", relayHost, daemonID)
	fmt.Println(bold.Padding(0, 2).Render("Connect other agents"))
	fmt.Println(dim.Padding(0, 2).Render("  Copy the following to your AI agent:"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 4).Render("I'd like to set up Clawvisor. Please navigate to the following URL and follow the setup instructions:"))
	fmt.Println(green.Padding(0, 4).Render(setupURL))
	fmt.Println()
}
