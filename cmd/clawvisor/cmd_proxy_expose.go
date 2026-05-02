package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/internal/runtime/expose"
)

var (
	proxyExposeBind      string
	proxyExposeProxyPort int
	proxyExposeAPIPort   int
	proxyExposeAllowCIDR []string
	proxyExposeDetach    bool

	// Test seam: in tests this is replaced to point at the test daemon's
	// runtime proxy + API instead of the local clawvisor config.
	proxyExposeUpstreams = defaultExposeUpstreams
)

var proxyCmd = &cobra.Command{
	Use:   "proxy",
	Short: "Operate the clawvisor runtime proxy",
}

var proxyExposeCmd = &cobra.Command{
	Use:   "expose",
	Short: "Expose the runtime proxy and daemon API on a network address",
	Long: `Run two TCP forwarders that bridge the local clawvisor runtime proxy and
daemon API onto a network-routable bind address. Intended for docker-compose
isolation on a remote host or other off-box workloads that need to reach the
runtime proxy without clawvisor in the loop.

Both listeners apply a source-IP allowlist (default: loopback + RFC-1918) and
relay raw TCP — auth is enforced upstream (the proxy still requires a valid
agent token; the API still requires its own auth headers).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		proxyUpstream, apiUpstream, err := proxyExposeUpstreams()
		if err != nil {
			return err
		}
		cfg := expose.Config{
			BindAddr:      proxyExposeBind,
			ProxyPort:     proxyExposeProxyPort,
			APIPort:       proxyExposeAPIPort,
			ProxyUpstream: proxyUpstream,
			APIUpstream:   apiUpstream,
			AllowCIDRs:    proxyExposeAllowCIDR,
			Logf: func(format string, args ...any) {
				fmt.Fprintf(os.Stdout, format+"\n", args...)
			},
		}
		if proxyExposeDetach {
			return runProxyExposeDetached(cfg)
		}
		return runProxyExposeForeground(cmd.Context(), cfg)
	},
	SilenceUsage: true,
}

var proxyExposeStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a detached `clawvisor proxy expose` process",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := proxyExposePIDPath()
		if err != nil {
			return err
		}
		pid := readExposePIDFile(path)
		if pid <= 0 {
			return fmt.Errorf("no running expose process (pidfile %s missing or empty)", path)
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			return fmt.Errorf("find pid %d: %w", pid, err)
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			if errors.Is(err, os.ErrProcessDone) || strings.Contains(err.Error(), "process already finished") {
				_ = os.Remove(path)
				return fmt.Errorf("expose pid %d was not running; pidfile cleared", pid)
			}
			return fmt.Errorf("signal pid %d: %w", pid, err)
		}
		_ = os.Remove(path)
		fmt.Printf("Sent SIGTERM to expose pid %d\n", pid)
		return nil
	},
	SilenceUsage: true,
}

func runProxyExposeForeground(ctx context.Context, cfg expose.Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pidPath, err := proxyExposePIDPath()
	if err != nil {
		return err
	}
	if err := writeExposePIDFile(pidPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write pidfile %s: %v\n", pidPath, err)
	} else {
		defer os.Remove(pidPath)
	}

	return expose.Run(ctx, cfg, func(ep expose.Endpoints) {
		fmt.Printf("clawvisor proxy expose: ready (proxy=%s api=%s)\n", ep.ProxyAddr, ep.APIAddr)
	})
}

// runProxyExposeDetached re-execs `clawvisor proxy expose` in the background
// without --detach. The parent waits briefly for the child to print readiness
// (or exit), then returns. The child manages its own pidfile.
func runProxyExposeDetached(cfg expose.Config) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate clawvisor binary: %w", err)
	}
	args := []string{"proxy", "expose",
		"--bind", cfg.BindAddr,
		"--proxy-port", fmt.Sprintf("%d", cfg.ProxyPort),
		"--api-port", fmt.Sprintf("%d", cfg.APIPort),
	}
	for _, c := range cfg.AllowCIDRs {
		args = append(args, "--allow-cidr", c)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached expose: %w", err)
	}
	// Brief delay so the child prints its readiness/error before we return.
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: release detached process: %v\n", err)
	}
	pidPath, _ := proxyExposePIDPath()
	fmt.Printf("clawvisor proxy expose: detached (pid=%d, pidfile=%s)\n", cmd.Process.Pid, pidPath)
	return nil
}

func writeExposePIDFile(path string) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600)
}

func readExposePIDFile(path string) int {
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

func proxyExposePIDPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	dir := filepath.Join(home, ".clawvisor", "runtime-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return filepath.Join(dir, "expose.pid"), nil
}

// defaultExposeUpstreams reads the local clawvisor config and returns the
// proxy + API upstream addresses the forwarders should bridge to.
func defaultExposeUpstreams() (proxy, api string, err error) {
	cfg := loadLocalDockerRuntimeConfig()
	proxy = strings.TrimSpace(cfg.RuntimeProxy.ListenAddr)
	if proxy == "" {
		proxy = "127.0.0.1:25290"
	}
	api = strings.TrimSpace(cfg.Server.Addr())
	if api == "" || strings.HasPrefix(api, ":") {
		return "", "", errors.New("local clawvisor config: server host:port not configured")
	}
	return proxy, api, nil
}

func init() {
	proxyExposeCmd.Flags().StringVar(&proxyExposeBind, "bind", "0.0.0.0", "Bind address for both listeners")
	proxyExposeCmd.Flags().IntVar(&proxyExposeProxyPort, "proxy-port", 25291, "Port for the runtime-proxy listener")
	proxyExposeCmd.Flags().IntVar(&proxyExposeAPIPort, "api-port", 18791, "Port for the daemon-API listener")
	proxyExposeCmd.Flags().StringSliceVar(&proxyExposeAllowCIDR, "allow-cidr", nil,
		"Source CIDR allowlist (repeatable). Default: loopback + RFC-1918.")
	proxyExposeCmd.Flags().BoolVar(&proxyExposeDetach, "detach", false, "Run in the background and write a pidfile")

	proxyCmd.AddCommand(proxyExposeCmd)
	proxyCmd.AddCommand(proxyExposeStopCmd)
	rootCmd.AddCommand(proxyCmd)
}
