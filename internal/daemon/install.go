package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"text/template"
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
        <string>daemon</string>
        <string>run</string>
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
ExecStart={{.Binary}} daemon run
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

// Install writes a service definition so the daemon starts at login.
// If no config.yaml exists in ~/.clawvisor, the setup wizard runs first.
func Install() error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}
	if err := ensureSetup(dataDir); err != nil {
		return fmt.Errorf("setup: %w", err)
	}

	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return fmt.Errorf("resolving symlinks: %w", err)
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
		return fmt.Errorf("auto-install is supported on macOS and Linux; start the daemon manually with `clawvisor daemon run`")
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
	fmt.Println("  To start now: clawvisor daemon start")
	fmt.Println("  To stop:      clawvisor daemon stop")
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
	fmt.Println("  To start now: clawvisor daemon start")
	fmt.Println("  To stop:      clawvisor daemon stop")
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
func Start() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		if _, err := os.Stat(plistPath); os.IsNotExist(err) {
			return fmt.Errorf("daemon not installed; run `clawvisor daemon install` first")
		}
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
		return fmt.Errorf("use `clawvisor daemon run` to start the daemon on this platform")
	}
	return nil
}

// Stop deactivates the running daemon service.
func Stop() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		out, err := exec.Command("launchctl", "unload", plistPath).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl unload: %s", string(out))
		}
		fmt.Println("  Daemon stopped.")
	case "linux":
		out, err := exec.Command("systemctl", "--user", "stop", "clawvisor.service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl stop: %s", string(out))
		}
		fmt.Println("  Daemon stopped.")
	default:
		return fmt.Errorf("use Ctrl+C to stop the foreground daemon on this platform")
	}
	return nil
}
