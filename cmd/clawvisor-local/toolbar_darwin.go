//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	localdaemon "github.com/clawvisor/clawvisor/internal/local/daemon"
	localproxy "github.com/clawvisor/clawvisor/internal/local/proxy"
	"github.com/clawvisor/clawvisor/pkg/version"
	"github.com/getlantern/systray"
	"github.com/spf13/cobra"
)

func toolbarCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "toolbar",
		Short: "Run the local daemon as a menu bar app",
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := localdaemon.New(baseDir())
			if err != nil {
				return err
			}
			if flagPort > 0 {
				d.OverridePort(flagPort)
			}
			return runToolbar(d)
		},
	}
}

func runToolbar(d *localdaemon.Daemon) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		runErr     error
		errMu      sync.Mutex
		done       = make(chan struct{})
		updateOnce sync.Mutex
	)

	controller := newKeepAwakeController(os.Getpid())
	updateEvents := make(chan toolbarUpdateEvent, 8)

	onReady := func() {
		systray.SetTitle("")
		systray.SetTooltip("Clawvisor Local Daemon")
		systray.SetTemplateIcon(toolbarDisconnectedIcon, toolbarDisconnectedIcon)

		statusItem := systray.AddMenuItem("Starting local daemon...", "")
		statusItem.Disable()

		connectionItem := systray.AddMenuItem("Pairing status: starting", "")
		connectionItem.Disable()

		serviceItem := systray.AddMenuItem("Services: 0", "")
		serviceItem.Disable()

		// Network Proxy lifecycle (Stage 2 M4 polish). The proxy is a
		// first-class daemon subsystem; this surfaces its state in the
		// menu bar and lets the user enable/disable/restart it without
		// dropping to the CLI. Hidden on first run until configured.
		proxyItem := systray.AddMenuItem("Network Proxy: not configured", "")
		proxyItem.Disable()
		proxyEnableItem := systray.AddMenuItem("Enable Proxy", "Start the configured Network Proxy")
		proxyEnableItem.Hide()
		proxyDisableItem := systray.AddMenuItem("Disable Proxy", "Stop the Network Proxy (keeps config)")
		proxyDisableItem.Hide()
		proxyRestartItem := systray.AddMenuItem("Restart Proxy", "Stop + restart the Network Proxy in place")
		proxyRestartItem.Hide()

		updateStatusItem := systray.AddMenuItem(toolbarUpdateStatusText(version.Check()), "")
		updateStatusItem.Disable()

		systray.AddSeparator()

		servicesMenu := systray.AddMenuItem("Services", "Include or exclude local services")
		reloadItem := systray.AddMenuItem("Reload Services", "Re-scan local services")
		checkUpdatesItem := systray.AddMenuItem("Check for Updates Now", "Download and restart if a newer release is available")
		autoUpdateItem := systray.AddMenuItemCheckbox("Automatically Install Updates", "Periodically check for and install new releases", d.AutoUpdateEnabled())
		keepAwakeItem := systray.AddMenuItemCheckbox("Keep Computer Awake", "Prevent idle sleep while Clawvisor Local is running", d.KeepAwakeEnabled())
		quitItem := systray.AddMenuItem("Quit", "Stop Clawvisor Local")

		var (
			serviceMenuItems   = make(map[string]*systray.MenuItem)
			serviceMenuStarted = make(map[string]bool)
			noServicesItem     *systray.MenuItem
		)

		refreshServicesMenu := func() {
			options := d.ServiceOptions()
			seen := make(map[string]bool, len(options))
			if len(options) == 0 {
				if noServicesItem == nil {
					noServicesItem = servicesMenu.AddSubMenuItem("No services found", "")
					noServicesItem.Disable()
				}
				noServicesItem.Show()
				return
			}
			if noServicesItem != nil {
				noServicesItem.Hide()
			}

			for _, opt := range options {
				seen[opt.Key] = true
				item, ok := serviceMenuItems[opt.Key]
				if !ok {
					if opt.Toggleable {
						item = servicesMenu.AddSubMenuItemCheckbox(opt.Name, opt.Tooltip, opt.Enabled)
					} else {
						item = servicesMenu.AddSubMenuItem(opt.Name, opt.Tooltip)
					}
					serviceMenuItems[opt.Key] = item
				}
				item.SetTitle(opt.Name)
				item.SetTooltip(opt.Tooltip)
				item.Show()
				if opt.Toggleable {
					item.Enable()
					if opt.Enabled {
						item.Check()
					} else {
						item.Uncheck()
					}
				} else {
					item.Disable()
				}
				if !opt.Toggleable || serviceMenuStarted[opt.Key] {
					continue
				}
				serviceMenuStarted[opt.Key] = true
				go func(serviceID, serviceName string, menuItem *systray.MenuItem) {
					for range menuItem.ClickedCh {
						enable := !menuItem.Checked()
						if err := d.SetServiceEnabled(serviceID, enable); err != nil {
							updateEvents <- toolbarUpdateEvent{status: "Could not save service setting"}
							continue
						}
						d.ReloadServices()
						action := "Disabled"
						if enable {
							action = "Enabled"
						}
						updateEvents <- toolbarUpdateEvent{
							status:           fmt.Sprintf("%s %s", action, serviceName),
							refreshServices:  true,
							refreshSummaries: true,
						}
					}
				}(opt.ID, opt.Name, item)
			}

			for key, item := range serviceMenuItems {
				if seen[key] {
					continue
				}
				item.Hide()
			}
		}

		version.SetAutoUpdate(d.AutoUpdateEnabled())

		if d.KeepAwakeEnabled() {
			if err := controller.Enable(); err != nil {
				statusItem.SetTitle("Keep-awake failed: " + err.Error())
				keepAwakeItem.Uncheck()
				_ = d.SetKeepAwake(false)
			}
		}

		startUpdateCheck := func(trigger string) {
			go func() {
				updateOnce.Lock()
				defer updateOnce.Unlock()

				updateEvents <- toolbarUpdateEvent{status: toolbarVersionCheckingText(version.GetCurrent())}

				info := version.CheckNow()
				if info.Latest == "" {
					updateEvents <- toolbarUpdateEvent{status: toolbarVersionStatusFallback(info.Current)}
					return
				}
				if !info.UpdateAvail {
					updateEvents <- toolbarUpdateEvent{status: toolbarUpdateStatusText(info)}
					return
				}

				oldVer, newVer, err := version.Apply(info.Latest)
				if err != nil {
					updateEvents <- toolbarUpdateEvent{status: "Update failed: " + err.Error()}
					return
				}

				err = restartUpdatedToolbar()
				if err != nil {
					updateEvents <- toolbarUpdateEvent{status: fmt.Sprintf("Updated to v%s, restart failed", newVer)}
					return
				}

				updateEvents <- toolbarUpdateEvent{
					status:  fmt.Sprintf("%s update: %s -> %s", trigger, toolbarVersionLabel(oldVer), toolbarVersionLabel(newVer)),
					restart: true,
				}
			}()
		}

		startAvailabilityProbe := func(parent context.Context) context.CancelFunc {
			probeCtx, probeCancel := context.WithCancel(parent)
			go func() {
				runProbe := func() {
					updateOnce.Lock()
					defer updateOnce.Unlock()

					info := version.CheckNow()
					if info.Latest == "" {
						updateEvents <- toolbarUpdateEvent{status: toolbarVersionStatusFallback(info.Current)}
						return
					}
					updateEvents <- toolbarUpdateEvent{status: toolbarUpdateStatusText(info)}
				}

				runProbe()

				ticker := time.NewTicker(toolbarVersionCheckInterval)
				defer ticker.Stop()

				for {
					select {
					case <-probeCtx.Done():
						return
					case <-ticker.C:
						runProbe()
					}
				}
			}()
			return probeCancel
		}

		startAutoUpdater := func(parent context.Context) context.CancelFunc {
			if !d.AutoUpdateEnabled() {
				return nil
			}

			autoCtx, autoCancel := context.WithCancel(parent)
			go func() {
				firstCheck := d.AutoUpdateInterval()
				if firstCheck > 5*time.Minute {
					firstCheck = 5 * time.Minute
				}
				if firstCheck <= 0 {
					firstCheck = 5 * time.Minute
				}

				timer := time.NewTimer(firstCheck)
				defer timer.Stop()

				for {
					select {
					case <-autoCtx.Done():
						return
					case <-timer.C:
						startUpdateCheck("Auto")
						timer.Reset(d.AutoUpdateInterval())
					}
				}
			}()
			return autoCancel
		}

		go func() {
			defer close(done)
			if err := d.RunContext(ctx); err != nil {
				errMu.Lock()
				runErr = err
				errMu.Unlock()
				statusItem.SetTitle("Daemon failed: " + err.Error())
				connectionItem.SetTitle("Pairing status: unavailable")
				serviceItem.SetTitle("Services: unavailable")
				controller.Disable()
			}
		}()

		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			autoCancel := startAutoUpdater(ctx)
			probeCancel := startAvailabilityProbe(ctx)
			defer func() {
				if autoCancel != nil {
					autoCancel()
				}
				if probeCancel != nil {
					probeCancel()
				}
			}()

			update := func() {
				summary := d.Summary()
				systray.SetTemplateIcon(toolbarIconFor(summary), toolbarIconFor(summary))
				statusItem.SetTitle(toolbarStatusText(summary))
				connectionItem.SetTitle(toolbarConnectionText(summary))
				serviceItem.SetTitle(fmt.Sprintf("Services: %d loaded, %d excluded", summary.ServiceCount, summary.ExcludedCount))

				// Proxy menu state — derived from the daemon's lifecycle
				// manager. Hidden actions when nothing's configured;
				// enable/disable visibility flips with state.
				if mgr := d.Proxy(); mgr != nil {
					s := mgr.Status()
					proxyItem.SetTitle(toolbarProxyText(s))
					configured := s.BridgeID != ""
					switch {
					case !configured:
						proxyEnableItem.Hide()
						proxyDisableItem.Hide()
						proxyRestartItem.Hide()
					case s.Enabled && (s.State == "running" || s.State == "starting"):
						proxyEnableItem.Hide()
						proxyDisableItem.Show()
						proxyRestartItem.Show()
					default:
						proxyEnableItem.Show()
						proxyDisableItem.Hide()
						proxyRestartItem.Hide()
					}
				}
			}

			update()
			refreshServicesMenu()

			for {
				select {
				case <-ticker.C:
					update()
				case ev := <-updateEvents:
					updateStatusItem.SetTitle(ev.status)
					if ev.refreshSummaries {
						update()
					}
					if ev.refreshServices {
						refreshServicesMenu()
					}
					if ev.restart {
						cancel()
						controller.Disable()
						systray.Quit()
						return
					}
				case <-reloadItem.ClickedCh:
					d.ReloadServices()
					refreshServicesMenu()
					update()
				case <-proxyEnableItem.ClickedCh:
					if mgr := d.Proxy(); mgr != nil {
						if err := mgr.Enable(); err != nil {
							updateStatusItem.SetTitle("Proxy enable: " + err.Error())
						}
					}
					update()
				case <-proxyDisableItem.ClickedCh:
					if mgr := d.Proxy(); mgr != nil {
						if err := mgr.Disable(); err != nil {
							updateStatusItem.SetTitle("Proxy disable: " + err.Error())
						}
					}
					update()
				case <-proxyRestartItem.ClickedCh:
					if mgr := d.Proxy(); mgr != nil {
						if err := mgr.Restart(); err != nil {
							updateStatusItem.SetTitle("Proxy restart: " + err.Error())
						}
					}
					update()
				case <-checkUpdatesItem.ClickedCh:
					startUpdateCheck("Manual")
				case <-autoUpdateItem.ClickedCh:
					enabled := !d.AutoUpdateEnabled()
					if err := d.SetAutoUpdateEnabled(enabled); err != nil {
						updateStatusItem.SetTitle("Could not save auto-update setting")
						continue
					}
					version.SetAutoUpdate(enabled)
					updateStatusItem.SetTitle(toolbarUpdateStatusText(version.Check()))
					if enabled {
						autoUpdateItem.Check()
						if autoCancel == nil {
							autoCancel = startAutoUpdater(ctx)
						}
					} else {
						autoUpdateItem.Uncheck()
						if autoCancel != nil {
							autoCancel()
							autoCancel = nil
						}
					}
				case <-keepAwakeItem.ClickedCh:
					enabled := !controller.Enabled()
					if err := d.SetKeepAwake(enabled); err != nil {
						statusItem.SetTitle("Could not save keep-awake setting")
						update()
						continue
					}
					if enabled {
						if err := controller.Enable(); err != nil {
							statusItem.SetTitle("Keep-awake failed: " + err.Error())
							_ = d.SetKeepAwake(false)
							keepAwakeItem.Uncheck()
							update()
							continue
						}
						keepAwakeItem.Check()
					} else {
						controller.Disable()
						keepAwakeItem.Uncheck()
					}
					update()
				case <-quitItem.ClickedCh:
					cancel()
					controller.Disable()
					systray.Quit()
					return
				case <-done:
					systray.Quit()
					return
				}
			}
		}()
	}

	onExit := func() {
		cancel()
		controller.Disable()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}

	systray.Run(onReady, onExit)

	errMu.Lock()
	defer errMu.Unlock()
	return runErr
}

type toolbarUpdateEvent struct {
	status           string
	restart          bool
	refreshServices  bool
	refreshSummaries bool
}

const toolbarVersionCheckInterval = time.Hour

func toolbarIconFor(summary localdaemon.StatusSummary) []byte {
	if summary.Connected {
		return toolbarConnectedIcon
	}
	return toolbarDisconnectedIcon
}

func toolbarStatusText(summary localdaemon.StatusSummary) string {
	if summary.UptimeSeconds <= 0 {
		return "Local daemon is starting..."
	}

	uptime := time.Duration(summary.UptimeSeconds) * time.Second
	if uptime >= time.Hour {
		return fmt.Sprintf("Local daemon running for %dh %dm", int(uptime.Hours()), int(uptime.Minutes())%60)
	}
	return fmt.Sprintf("Local daemon running for %dm", int(uptime.Minutes()))
}

func toolbarConnectionText(summary localdaemon.StatusSummary) string {
	switch {
	case summary.Connected:
		host := strings.TrimPrefix(summary.CloudOrigin, "https://")
		host = strings.TrimPrefix(host, "http://")
		if host == "app.clawvisor.com" {
			host = "Clawvisor"
		}
		if host == "" {
			host = "cloud"
		}
		return "Connected to " + host
	case summary.Paired:
		return "Paired, waiting to reconnect"
	default:
		return "Not paired yet"
	}
}

// toolbarProxyText is the menu-bar one-liner for the Network Proxy
// state. Compact: "Network Proxy: <state> on :<port>" when running,
// "<state>" otherwise. Compatible with proxy.Status's State enum.
func toolbarProxyText(s localproxy.Status) string {
	if s.BridgeID == "" {
		return "Network Proxy: not configured"
	}
	switch s.State {
	case "running":
		return fmt.Sprintf("Network Proxy: ● running on :%d (%s)", s.ListenPort, s.Mode)
	case "starting":
		return "Network Proxy: ◌ starting…"
	case "stopped", "disabled":
		return "Network Proxy: ○ disabled"
	case "failed":
		if s.LastError != "" {
			return fmt.Sprintf("Network Proxy: ✕ failed (%s)", s.LastError)
		}
		return "Network Proxy: ✕ failed"
	default:
		return "Network Proxy: " + string(s.State)
	}
}

func toolbarUpdateStatusText(info *version.Info) string {
	if info == nil {
		return toolbarVersionStatusFallback(version.GetCurrent())
	}
	text := "Version " + toolbarVersionLabel(info.Current)
	if info.UpdateAvail {
		text += " (update available)"
	}
	return text
}

func toolbarVersionCheckingText(currentVersion string) string {
	return "Version " + toolbarVersionLabel(currentVersion) + " (checking...)"
}

func toolbarVersionStatusFallback(currentVersion string) string {
	return "Version " + toolbarVersionLabel(currentVersion)
}

func toolbarVersionLabel(currentVersion string) string {
	currentVersion = strings.TrimSpace(currentVersion)
	switch currentVersion {
	case "", "dev":
		return "dev build"
	default:
		return "v" + strings.TrimPrefix(currentVersion, "v")
	}
}

func restartUpdatedToolbar() error {
	if toolbarLaunchAgentInstalled() {
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}

	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

func toolbarLaunchAgentInstalled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, "Library", "LaunchAgents", "com.clawvisor.local.plist"))
	return err == nil
}

type keepAwakeController struct {
	pid int

	mu  sync.Mutex
	cmd *exec.Cmd
}

func newKeepAwakeController(pid int) *keepAwakeController {
	return &keepAwakeController{pid: pid}
}

func (c *keepAwakeController) Enable() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cmd != nil {
		return nil
	}

	cmd := exec.Command("/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(c.pid))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting caffeinate: %w", err)
	}

	c.cmd = cmd
	go func(cmd *exec.Cmd) {
		_ = cmd.Wait()
		c.mu.Lock()
		if c.cmd == cmd {
			c.cmd = nil
		}
		c.mu.Unlock()
	}(cmd)

	return nil
}

func (c *keepAwakeController) Disable() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cmd == nil || c.cmd.Process == nil {
		return
	}

	_ = c.cmd.Process.Kill()
	c.cmd = nil
}

func (c *keepAwakeController) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cmd != nil
}
