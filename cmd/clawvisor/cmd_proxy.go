package main

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Proxy subcommands — "clawvisor proxy …" — own the end-user flow for
// the Network Proxy: install as a clawvisor-local supervised service,
// scope a single command's HTTP traffic through it ("run"), and
// install the TLS CA cert into the system trust store ("trust-ca").
//
// See docs/design-proxy-stage2.md §M4 and the follow-on UX work.

//go:embed assets/proxy-service/*
var proxyServiceAssets embed.FS

var proxyCmd = &cobra.Command{
	Use:   "proxy",
	Short: "Manage the Clawvisor Network Proxy (observation + credential injection)",
	Long: `Install, configure, and launch agents through the Clawvisor Network Proxy.

The proxy MITM-intercepts TLS so Clawvisor can observe LLM API traffic,
inject vault credentials, and enforce policies. It runs as a supervised
service under clawvisor-local, so it restarts on crash and appears in
the toolbar. Scoped per-agent: "clawvisor proxy run <cmd>" routes only
that one command's traffic through the proxy.`,
}

var proxyInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the proxy as a clawvisor-local supervised service",
	Long: `Writes a service manifest into ~/.clawvisor/local/services/clawvisor.network-proxy/
so the local daemon starts, health-checks, and restarts the proxy. Also
copies in the proxy binary and writes the Clawvisor integration config
(server_url, proxy_token, bridge_id).

Run 'clawvisor proxy install --help' to see the required flags.`,
	SilenceUsage: true,
	RunE:         runProxyInstall,
}

var (
	installBinaryPath string
	installServerURL  string
	installProxyToken string
	installBridgeID   string
	installListenPort string
)

func init() {
	proxyInstallCmd.Flags().StringVar(&installBinaryPath, "binary", "",
		"Path to the proxy binary (the 'kumo' / clawvisor-proxy executable). Required.")
	proxyInstallCmd.Flags().StringVar(&installServerURL, "server-url", "http://127.0.0.1:25297",
		"Clawvisor server URL the proxy should register with.")
	proxyInstallCmd.Flags().StringVar(&installProxyToken, "proxy-token", "",
		"cvisproxy_... token minted from the dashboard's 'Enable Proxy' flow. Required.")
	proxyInstallCmd.Flags().StringVar(&installBridgeID, "bridge-id", "",
		"Bridge UUID this proxy serves. Required.")
	// Default port sits in the Clawvisor service family (25297 = server,
	// 25298 = proxy, 25299 = local daemon pairing). Far enough from the
	// usual 8080/8443/9000 minefield that conflicts are rare; contiguous
	// with the other daemons so users can remember the range.
	proxyInstallCmd.Flags().StringVar(&installListenPort, "listen-port", "25298",
		"TCP port the proxy should listen on (127.0.0.1).")

	proxyCmd.AddCommand(proxyInstallCmd)
	proxyCmd.AddCommand(proxyRunCmd)
	proxyCmd.AddCommand(proxyTrustCACmd)
	proxyCmd.AddCommand(proxyUninstallCmd)
	rootCmd.AddCommand(proxyCmd)
}

// proxyServiceDir returns ~/.clawvisor/local/services/clawvisor.network-proxy.
func proxyServiceDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".clawvisor", "local", "services", "clawvisor.network-proxy"), nil
}

// proxyDataDir returns the proxy's persistent state directory
// (CA cert, signing keys, traffic log).
func proxyDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".clawvisor", "proxy-data"), nil
}

func runProxyInstall(cmd *cobra.Command, args []string) error {
	if installBinaryPath == "" {
		return errors.New("--binary is required (path to the proxy executable)")
	}
	if installProxyToken == "" || installBridgeID == "" {
		return errors.New("--proxy-token and --bridge-id are required (copy from the dashboard's Enable Proxy flow)")
	}

	srcBin, err := filepath.Abs(installBinaryPath)
	if err != nil {
		return fmt.Errorf("resolve --binary: %w", err)
	}
	if _, err := os.Stat(srcBin); err != nil {
		return fmt.Errorf("--binary %s: %w", srcBin, err)
	}

	// Pre-flight: is something already bound to the proxy port? If yes,
	// the supervised proxy will crash-loop — better to fail fast here
	// with a clear error than to leave the user staring at an unhealthy
	// service in the toolbar.
	if owner := portOwner("127.0.0.1:" + installListenPort); owner != "" {
		return fmt.Errorf("port %s is already in use (%s). Stop that process or rerun with --listen-port <other>",
			installListenPort, owner)
	}

	dir, err := proxyServiceDir()
	if err != nil {
		return err
	}
	dataDir, err := proxyDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create service dir: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	dstBin := filepath.Join(dir, "clawvisor-proxy")
	if err := copyFile(srcBin, dstBin, 0755); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}

	// Integration config — identical shape to docs/proxy-api.md.
	cfg := fmt.Sprintf("server_url: %q\nproxy_token: %q\nbridge_id: %q\n",
		installServerURL, installProxyToken, installBridgeID)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0600); err != nil {
		return fmt.Errorf("write config.yaml: %w", err)
	}

	// Unpack embedded templates (service.yaml, run.sh) — the run.sh
	// interpolates paths so the service survives moves of the user's
	// home directory.
	if err := writeEmbeddedAsset("assets/proxy-service/service.yaml", filepath.Join(dir, "service.yaml"), 0644, map[string]string{
		"PLATFORM":    runtime.GOOS,
		"LISTEN_PORT": installListenPort,
	}); err != nil {
		return err
	}
	if err := writeEmbeddedAsset("assets/proxy-service/run.sh", filepath.Join(dir, "run.sh"), 0755, map[string]string{
		"DATA_DIR":    dataDir,
		"LISTEN_PORT": installListenPort,
	}); err != nil {
		return err
	}

	fmt.Printf("Installed proxy service at %s\n", dir)
	fmt.Printf("Proxy data dir: %s\n", dataDir)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Tell clawvisor-local to pick up the new service:")
	fmt.Println("       curl -sX POST http://127.0.0.1:25299/api/services/reload")
	fmt.Println("     (or quit and relaunch the toolbar app.)")
	fmt.Println("  2. Install the TLS CA cert so agents trust it:")
	fmt.Println("       clawvisor proxy trust-ca")
	fmt.Println("  3. Run an agent through the proxy:")
	fmt.Println("       clawvisor proxy run --agent-token cvis_XXX -- claude-code")
	return nil
}

// writeEmbeddedAsset unpacks one of the embedded templates, substituting
// {{VAR}} placeholders from vars. Kept minimal — text/template would be
// overkill for <1KB files.
func writeEmbeddedAsset(embedPath, dst string, mode os.FileMode, vars map[string]string) error {
	raw, err := proxyServiceAssets.ReadFile(embedPath)
	if err != nil {
		return fmt.Errorf("embed %s: %w", embedPath, err)
	}
	body := string(raw)
	for k, v := range vars {
		body = strings.ReplaceAll(body, "{{"+k+"}}", v)
	}
	return os.WriteFile(dst, []byte(body), mode)
}

func copyFile(src, dst string, mode os.FileMode) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer df.Close()
	_, err = io.Copy(df, sf)
	return err
}

// -- clawvisor proxy run --------------------------------------------------

var (
	runAgentToken string
	runListenHost string
	runListenPort string
)

var proxyRunCmd = &cobra.Command{
	Use:   "run [flags] -- <command> [args...]",
	Short: "Launch a command with its HTTP traffic scoped through the proxy",
	Long: `Sets HTTP_PROXY, HTTPS_PROXY, and NODE_EXTRA_CA_CERTS only for the
child process, so the rest of your shell is untouched.

Example:
  clawvisor proxy run --agent-token cvis_abc -- claude-code
  clawvisor proxy run --agent-token cvis_abc -- curl https://api.anthropic.com/...

Only the invoked command (and its descendants) flow through the proxy.
Your browser, git, brew, etc. are unaffected.`,
	SilenceUsage: true,
	RunE:         runProxyRun,
}

func init() {
	proxyRunCmd.Flags().StringVar(&runAgentToken, "agent-token", "",
		"cvis_... token to authenticate as. Defaults to $CLAWVISOR_AGENT_TOKEN.")
	proxyRunCmd.Flags().StringVar(&runListenHost, "host", "127.0.0.1",
		"Proxy host the child process should target.")
	proxyRunCmd.Flags().StringVar(&runListenPort, "port", "25298",
		"Proxy port the child process should target. Default matches 'clawvisor proxy install'.")
}

func runProxyRun(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return errors.New("missing command. Example: clawvisor proxy run -- claude-code")
	}
	token := runAgentToken
	if token == "" {
		token = os.Getenv("CLAWVISOR_AGENT_TOKEN")
	}
	if token == "" {
		return errors.New("no agent token — pass --agent-token or set CLAWVISOR_AGENT_TOKEN")
	}

	dataDir, err := proxyDataDir()
	if err != nil {
		return err
	}
	caPath := filepath.Join(dataDir, "ca.pem")
	if _, err := os.Stat(caPath); err != nil {
		return fmt.Errorf("CA cert not found at %s (run the proxy once so it generates, then try 'clawvisor proxy trust-ca'): %w", caPath, err)
	}

	proxyURL := fmt.Sprintf("http://%s@%s:%s", token, runListenHost, runListenPort)

	c := exec.Command(args[0], args[1:]...) //nolint:gosec
	c.Env = append(os.Environ(),
		"HTTP_PROXY="+proxyURL,
		"HTTPS_PROXY="+proxyURL,
		"http_proxy="+proxyURL,
		"https_proxy="+proxyURL,
		"NO_PROXY=localhost,127.0.0.1,::1",
		"no_proxy=localhost,127.0.0.1,::1",
		"NODE_EXTRA_CA_CERTS="+caPath,
		"SSL_CERT_FILE="+caPath,
		"REQUESTS_CA_BUNDLE="+caPath,
		"CLAWVISOR_PROXY="+proxyURL,
	)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		// Preserve the child's exit code if possible.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

// -- clawvisor proxy trust-ca --------------------------------------------

var proxyTrustCACmd = &cobra.Command{
	Use:   "trust-ca",
	Short: "Install the proxy's TLS CA cert into the system trust store",
	Long: `macOS: adds the cert to the user login keychain (you'll get a password
prompt). Linux: copies to /usr/local/share/ca-certificates/ and runs
update-ca-certificates (needs sudo).

After this, tools that use the system trust store (curl, Python requests,
Go net/http, etc.) will trust the proxy's MITM certificates. Node fetch()
still needs NODE_EXTRA_CA_CERTS — 'clawvisor proxy run' sets that for
child processes automatically.`,
	SilenceUsage: true,
	RunE:         runProxyTrustCA,
}

func runProxyTrustCA(cmd *cobra.Command, args []string) error {
	dataDir, err := proxyDataDir()
	if err != nil {
		return err
	}
	caPath := filepath.Join(dataDir, "ca.pem")
	if _, err := os.Stat(caPath); err != nil {
		return fmt.Errorf("CA cert not found at %s (make sure the proxy has run at least once): %w", caPath, err)
	}

	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		keychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
		// -r trustRoot marks it as trusted for SSL; keychain-scoped to the
		// user so this doesn't require sudo. macOS will prompt the user
		// with a GUI password dialog.
		c := exec.Command("security", "add-trusted-cert",
			"-r", "trustRoot",
			"-k", keychain,
			caPath,
		)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("security add-trusted-cert failed: %w", err)
		}
		fmt.Println("Added Clawvisor Proxy CA to your login keychain.")
		fmt.Println("To remove later: security remove-trusted-cert ~/.clawvisor/proxy-data/ca.pem")
		return nil

	case "linux":
		dst := "/usr/local/share/ca-certificates/clawvisor-proxy.crt"
		fmt.Printf("Installing CA to %s (will prompt for sudo)\n", dst)
		c := exec.Command("sudo", "sh", "-c",
			fmt.Sprintf("cp %q %q && update-ca-certificates", caPath, dst))
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()

	default:
		return fmt.Errorf("trust-ca is not yet implemented for %s — install %s manually", runtime.GOOS, caPath)
	}
}

// -- clawvisor proxy uninstall -------------------------------------------

var proxyUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the supervised proxy service",
	Long:  "Stops the clawvisor-local-supervised proxy, removes its service manifest, and preserves the data dir (CA, logs) so reinstall keeps the same CA.",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := proxyServiceDir()
		if err != nil {
			return err
		}
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			fmt.Println("No proxy service installed.")
			return nil
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
		fmt.Printf("Removed %s\n", dir)
		fmt.Println("Run: curl -sX POST http://127.0.0.1:25299/api/services/reload")
		fmt.Println("(data dir ~/.clawvisor/proxy-data preserved; delete manually to reset CA)")
		return nil
	},
}

// -- helpers -------------------------------------------------------------

// envvarBlock is a debug aid — prints the env vars "run" would set.
// Not wired as a public subcommand yet; callers use the bytes.Buffer API.
func envvarBlock(token, host, port, caPath string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "export HTTP_PROXY=http://%s@%s:%s\n", token, host, port)
	fmt.Fprintf(&b, "export HTTPS_PROXY=\"$HTTP_PROXY\"\n")
	fmt.Fprintf(&b, "export NODE_EXTRA_CA_CERTS=%q\n", caPath)
	fmt.Fprintf(&b, "export SSL_CERT_FILE=%q\n", caPath)
	return b.String()
}

var _ = envvarBlock // reserved for future 'clawvisor proxy env' subcommand

// portOwner returns a short description of what's listening on addr,
// or "" if nothing is. Non-blocking best-effort — uses a short-timeout
// dial + an lsof fallback for the human-readable process name.
func portOwner(addr string) string {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return ""
	}
	_ = conn.Close()
	// Try to name the owning process via lsof; if unavailable, fall
	// back to "an existing process".
	parts := strings.Split(addr, ":")
	port := parts[len(parts)-1]
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN").Output()
	if err == nil {
		for i, line := range strings.Split(string(out), "\n") {
			if i == 0 || line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) > 0 {
				return fields[0] + " (PID " + func() string {
					if len(fields) > 1 {
						return fields[1]
					}
					return "?"
				}() + ")"
			}
		}
	}
	return "an existing process"
}
