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
		return installLaunchd(home, data)
	case "linux":
		return installSystemd(home, data)
	default:
		return fmt.Errorf("auto-install is supported on macOS and Linux; start the daemon manually with `clawvisor start`")
	}
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
