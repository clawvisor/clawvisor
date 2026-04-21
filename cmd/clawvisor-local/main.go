package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/internal/local/config"
	localdaemon "github.com/clawvisor/clawvisor/internal/local/daemon"
	"github.com/clawvisor/clawvisor/internal/local/executor"
	"github.com/clawvisor/clawvisor/internal/local/services"
	"github.com/clawvisor/clawvisor/internal/local/state"
	"github.com/clawvisor/clawvisor/pkg/version"
)

var (
	flagConfigDir string
	flagPort      int
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "clawvisor-local",
		Short: "Clawvisor local daemon",
		Long:  "A lightweight daemon that bridges local services to the Clawvisor cloud.",
		RunE:  runDaemon,
	}

	rootCmd.PersistentFlags().StringVar(&flagConfigDir, "config", "", "config directory (default: ~/.clawvisor/local)")
	rootCmd.PersistentFlags().IntVar(&flagPort, "port", 0, "override listen port")

	rootCmd.AddCommand(
		statusCmd(),
		unpairCmd(),
		reloadCmd(),
		toolbarCmd(),
		servicesParentCmd(),
		daemonParentCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func baseDir() string {
	if flagConfigDir != "" {
		return flagConfigDir
	}
	return config.BaseDir()
}

func runDaemon(cmd *cobra.Command, args []string) error {
	d, err := localdaemon.New(baseDir())
	if err != nil {
		return err
	}
	if flagPort > 0 {
		d.OverridePort(flagPort)
	}
	return d.Run()
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print pairing status, connection state, services",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := baseDir()
			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}

			// Try to reach the running daemon for live status.
			liveURL := fmt.Sprintf("http://127.0.0.1:%d/api/status", cfg.Port)
			if body, err := httpGet(liveURL); err == nil {
				return printLiveStatus(body)
			}

			// Fallback: offline status from local state + service scan.
			st, err := state.Load(dir)
			if err != nil {
				return err
			}

			result := services.Discover(cfg.ServiceDirs, cfg.DefaultTimeout.Duration, disabledServiceSet(cfg.DisabledServices))

			fmt.Printf("Daemon ID:    %s\n", st.DaemonID)
			fmt.Printf("Name:         %s\n", cfg.Name)
			fmt.Printf("Version:      %s\n", version.Version)
			fmt.Printf("Connected:    no (daemon not running)\n")

			if st.IsPaired() {
				fmt.Printf("Paired:       yes (since %s)\n", st.PairedAt.Format("2006-01-02"))
				fmt.Printf("Cloud:        %s\n", st.CloudOrigin)
			} else {
				fmt.Printf("Paired:       no\n")
			}

			fmt.Printf("Services:     %d loaded, %d excluded\n\n", len(result.Services), len(result.Excluded))

			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			for _, svc := range result.Services {
				fmt.Fprintf(w, "  %s\t%s\t%d actions\t%s\n", svc.ID, svc.Name, len(svc.Actions), svc.Type)
			}
			w.Flush()

			if len(result.Excluded) > 0 {
				fmt.Println("\nExcluded:")
				for _, e := range result.Excluded {
					fmt.Printf("  [%s] %s — %s\n", e.Category, e.Path, e.Error)
				}
			}

			return nil
		},
	}
}

type liveStatus struct {
	DaemonID      string        `json:"daemon_id"`
	Name          string        `json:"name"`
	Version       string        `json:"version"`
	Paired        bool          `json:"paired"`
	Connected     bool          `json:"connected"`
	CloudOrigin   string        `json:"cloud_origin"`
	UptimeSeconds int           `json:"uptime_seconds"`
	Services      []liveService `json:"services"`
	Excluded      []struct {
		Category string `json:"category"`
		Path     string `json:"path"`
		Error    string `json:"error"`
	} `json:"excluded_services"`
}

type liveService struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Actions []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"actions"`
}

func printLiveStatus(body string) error {
	var status liveStatus
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		return fmt.Errorf("parsing status response: %w", err)
	}

	fmt.Printf("Daemon ID:    %s\n", status.DaemonID)
	fmt.Printf("Name:         %s\n", status.Name)
	fmt.Printf("Version:      %s\n", status.Version)

	if status.Paired {
		fmt.Printf("Paired:       yes\n")
		fmt.Printf("Connected:    %v (to %s)\n", status.Connected, status.CloudOrigin)
	} else {
		fmt.Printf("Paired:       no\n")
		fmt.Printf("Connected:    no\n")
	}

	hours := status.UptimeSeconds / 3600
	minutes := (status.UptimeSeconds % 3600) / 60
	if hours > 0 {
		fmt.Printf("Uptime:       %dh %dm\n", hours, minutes)
	} else {
		fmt.Printf("Uptime:       %dm\n", minutes)
	}

	fmt.Printf("Services:     %d loaded, %d excluded\n\n", len(status.Services), len(status.Excluded))

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	for _, svc := range status.Services {
		typeStr := svc.Type
		if svc.Type == "server" && svc.Status != "" {
			typeStr = fmt.Sprintf("server (%s)", svc.Status)
		}
		fmt.Fprintf(w, "  %s\t%s\t%d actions\t%s\n", svc.ID, svc.Name, len(svc.Actions), typeStr)
	}
	w.Flush()

	if len(status.Excluded) > 0 {
		fmt.Println("\nExcluded:")
		for _, e := range status.Excluded {
			fmt.Printf("  [%s] %s — %s\n", e.Category, e.Path, e.Error)
		}
	}

	return nil
}

func httpGet(url string) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func servicesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List discovered services and their actions",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := baseDir()
			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}

			result := services.Discover(cfg.ServiceDirs, cfg.DefaultTimeout.Duration, disabledServiceSet(cfg.DisabledServices))

			for _, svc := range result.Services {
				header := fmt.Sprintf("%s (%s)", svc.ID, svc.Name)
				if svc.Type == "server" {
					header += "  [server]"
				}
				fmt.Println(header)

				w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
				for _, a := range svc.Actions {
					paramStrs := make([]string, 0, len(a.Params))
					for _, p := range a.Params {
						s := p.Name
						if p.Required {
							s += "*"
						}
						paramStrs = append(paramStrs, s)
					}
					params := "(none)"
					if len(paramStrs) > 0 {
						params = strings.Join(paramStrs, ", ")
					}
					fmt.Fprintf(w, "  %s\t%s\tparams: %s\n", a.ID, a.Name, params)
				}
				w.Flush()
				fmt.Println()
			}

			return nil
		},
	}
}

func unpairCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unpair",
		Short: "Clear pairing state, disconnect from cloud",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := baseDir()
			st, err := state.Load(dir)
			if err != nil {
				return err
			}

			if err := state.Clear(dir, st.DaemonID); err != nil {
				return err
			}

			fmt.Println("Pairing state cleared.")
			return nil
		},
	}
}

func reloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Re-scan services directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := baseDir()
			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}

			// Send reload request to running daemon.
			url := fmt.Sprintf("http://127.0.0.1:%d/api/services/reload", cfg.Port)
			resp, err := httpPost(url)
			if err != nil {
				return fmt.Errorf("reload failed (is the daemon running?): %w", err)
			}
			fmt.Println(resp)
			return nil
		},
	}
}

func validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [PATH]",
		Short: "Validate a service.yaml file or all discovered services",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := baseDir()
			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}

			if len(args) > 0 {
				// Validate a single file.
				path := args[0]
				svc, err := services.ParseManifest(path, cfg.DefaultTimeout.Duration)
				if err != nil {
					fmt.Printf("✗ %s\n  Error: %s\n", path, err)
					os.Exit(1)
				}
				fmt.Printf("✓ service.yaml is valid\n")
				fmt.Printf("  Name: %s\n", svc.Name)
				fmt.Printf("  Type: %s\n", svc.Type)
				actionIDs := make([]string, len(svc.Actions))
				for i, a := range svc.Actions {
					actionIDs[i] = a.ID
				}
				fmt.Printf("  Actions: %d (%s)\n", len(svc.Actions), strings.Join(actionIDs, ", "))

				// Check for warnings.
				for _, a := range svc.Actions {
					if svc.Type == "exec" && len(a.Run) > 0 {
						runPath := a.Run[0]
						if _, statErr := os.Stat(runPath); statErr == nil {
							info, _ := os.Stat(runPath)
							if info != nil && info.Mode()&0111 == 0 {
								fmt.Printf("  Warnings:\n    - Action %q: %s is not executable (chmod +x?)\n", a.ID, runPath)
							}
						}
					}
				}
				return nil
			}

			// Validate all discovered services.
			result := services.Discover(cfg.ServiceDirs, cfg.DefaultTimeout.Duration, disabledServiceSet(cfg.DisabledServices))
			hasErrors := false

			for _, svc := range result.Services {
				fmt.Printf("✓ %s — %d actions\n", svc.ID, len(svc.Actions))
			}
			for _, e := range result.Excluded {
				switch e.Category {
				case "unsupported":
					fmt.Printf("~ %s [%s] — %s\n", e.Path, e.Category, e.Error)
				default:
					fmt.Printf("✗ %s [%s] — %s\n", e.Path, e.Category, e.Error)
					hasErrors = true
				}
			}

			if hasErrors {
				os.Exit(1)
			}
			return nil
		},
	}
}

func runCmd() *cobra.Command {
	var params []string

	cmd := &cobra.Command{
		Use:   "run SERVICE ACTION",
		Short: "Manually invoke an action (for testing)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			serviceID := args[0]
			actionID := args[1]

			dir := baseDir()
			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}

			// Discover services.
			result := services.Discover(cfg.ServiceDirs, cfg.DefaultTimeout.Duration, disabledServiceSet(cfg.DisabledServices))
			registry := services.NewRegistry()
			registry.Load(result)

			svc, action := registry.GetAction(serviceID, actionID)
			if svc == nil {
				return fmt.Errorf("unknown service: %s", serviceID)
			}
			if action == nil {
				return fmt.Errorf("unknown action: %s.%s", serviceID, actionID)
			}

			// Parse --param key=value pairs.
			paramMap := make(map[string]string)
			for _, p := range params {
				parts := strings.SplitN(p, "=", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid param format: %q (expected key=value)", p)
				}
				paramMap[parts[0]] = parts[1]
			}

			if svc.Type == "exec" {
				resp := executor.RunExec(context.Background(), svc, action, paramMap, cfg.Env, cfg.MaxOutputSize, "local")
				out, _ := json.MarshalIndent(resp, "", "  ")
				fmt.Println(string(out))
			} else {
				// Server mode: start the server, dispatch, stop.
				serverMgr := executor.NewServerManager(dir)
				if err := serverMgr.Init(); err != nil {
					return err
				}
				serverMgr.Register(svc)
				defer serverMgr.StopAll()

				sp := serverMgr.Get(svc.ID)
				resp := sp.Dispatch(context.Background(), action, paramMap, cfg.MaxOutputSize)
				out, _ := json.MarshalIndent(resp, "", "  ")
				fmt.Println(string(out))
			}

			return nil
		},
	}

	cmd.Flags().StringArrayVar(&params, "param", nil, "action parameter (key=value)")

	return cmd
}

func installServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install clawvisor-local as a launchd (macOS) / systemd (Linux) service",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch runtime.GOOS {
			case "darwin":
				return installLaunchd()
			case "linux":
				return installSystemd()
			default:
				return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
			}
		},
	}
}

func uninstallServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the launchd / systemd service for clawvisor-local",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch runtime.GOOS {
			case "darwin":
				return uninstallLaunchd()
			case "linux":
				return uninstallSystemd()
			default:
				return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
			}
		},
	}
}

func installLaunchd() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}

	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, ".clawvisor", "local", "daemon.log")
	plistPath := localPlistPath()
	plistDir := filepath.Dir(plistPath)

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(plistDir, 0755); err != nil {
		return err
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.clawvisor.local</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>toolbar</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, exePath, logPath, logPath)

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		return fmt.Errorf("writing plist: %w", err)
	}

	// Unload first so re-running install-service is idempotent.
	exec.Command("launchctl", "unload", plistPath).Run() //nolint:errcheck
	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		return fmt.Errorf("loading launchd service: %w", err)
	}

	fmt.Printf("Installed and started com.clawvisor.local\n")
	fmt.Printf("  Plist: %s\n", plistPath)
	fmt.Printf("  Log:   %s\n", logPath)
	return nil
}

func uninstallLaunchd() error {
	plistPath := localPlistPath()

	_ = exec.Command("launchctl", "unload", plistPath).Run()
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing plist: %w", err)
	}

	fmt.Println("Uninstalled com.clawvisor.local")
	return nil
}

func installSystemd() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}

	home, _ := os.UserHomeDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	unitPath := filepath.Join(unitDir, "clawvisor-local.service")

	if err := os.MkdirAll(unitDir, 0755); err != nil {
		return err
	}

	unit := fmt.Sprintf(`[Unit]
Description=Clawvisor Local Daemon
After=network.target

[Service]
ExecStart=%s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, exePath)

	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		return fmt.Errorf("writing unit file: %w", err)
	}

	if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("reloading systemd: %w", err)
	}
	if err := exec.Command("systemctl", "--user", "enable", "--now", "clawvisor-local").Run(); err != nil {
		return fmt.Errorf("enabling service: %w", err)
	}

	fmt.Printf("Installed and started clawvisor-local.service\n")
	fmt.Printf("  Unit: %s\n", unitPath)
	return nil
}

func uninstallSystemd() error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", "clawvisor-local").Run()

	home, _ := os.UserHomeDir()
	unitPath := filepath.Join(home, ".config", "systemd", "user", "clawvisor-local.service")

	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing unit file: %w", err)
	}

	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()

	fmt.Println("Uninstalled clawvisor-local.service")
	return nil
}

const localLaunchdLabel = "com.clawvisor.local"

func localPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", localLaunchdLabel+".plist")
}

func startServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the installed clawvisor-local service",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch runtime.GOOS {
			case "darwin":
				plistPath := localPlistPath()
				if _, err := os.Stat(plistPath); os.IsNotExist(err) {
					return fmt.Errorf("service not installed; run install-service first")
				}
				if out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput(); err != nil {
					return fmt.Errorf("launchctl load: %s", strings.TrimSpace(string(out)))
				}
			case "linux":
				if out, err := exec.Command("systemctl", "--user", "start", "clawvisor-local").CombinedOutput(); err != nil {
					return fmt.Errorf("systemctl start: %s", strings.TrimSpace(string(out)))
				}
			default:
				return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
			}
			fmt.Println("Service started.")
			return nil
		},
	}
}

func stopServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running clawvisor-local service",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch runtime.GOOS {
			case "darwin":
				plistPath := localPlistPath()
				if _, err := os.Stat(plistPath); os.IsNotExist(err) {
					return fmt.Errorf("service not installed; run install-service first")
				}
				if out, err := exec.Command("launchctl", "unload", plistPath).CombinedOutput(); err != nil {
					return fmt.Errorf("launchctl unload: %s", strings.TrimSpace(string(out)))
				}
			case "linux":
				if out, err := exec.Command("systemctl", "--user", "stop", "clawvisor-local").CombinedOutput(); err != nil {
					return fmt.Errorf("systemctl stop: %s", strings.TrimSpace(string(out)))
				}
			default:
				return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
			}
			fmt.Println("Service stopped.")
			return nil
		},
	}
}

func restartServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the clawvisor-local service",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch runtime.GOOS {
			case "darwin":
				plistPath := localPlistPath()
				if _, err := os.Stat(plistPath); os.IsNotExist(err) {
					return fmt.Errorf("service not installed; run install-service first")
				}
				exec.Command("launchctl", "unload", plistPath).Run() //nolint:errcheck
				if out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput(); err != nil {
					return fmt.Errorf("launchctl load: %s", strings.TrimSpace(string(out)))
				}
			case "linux":
				if out, err := exec.Command("systemctl", "--user", "restart", "clawvisor-local").CombinedOutput(); err != nil {
					return fmt.Errorf("systemctl restart: %s", strings.TrimSpace(string(out)))
				}
			default:
				return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
			}
			fmt.Println("Service restarted.")
			return nil
		},
	}
}

func httpPost(url string) (string, error) {
	resp, err := http.Post(url, "", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func disabledServiceSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		out[id] = true
	}
	return out
}
