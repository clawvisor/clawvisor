package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

//go:embed shim/clawvisor-proxy-shim.js
var nodeProxyShim []byte

// Proxy subcommands — "clawvisor-local proxy …" — drive the
// first-class Clawvisor Network Proxy lifecycle on the user's machine.
// The proxy is a dedicated daemon subsystem (internal/local/proxy),
// not a pluggable service; these commands are thin clients over the
// daemon's /api/proxy/* HTTP API on 127.0.0.1:25299.
//
// They live on clawvisor-local (not clawvisor) because the daemon is
// the binary cloud users install — they have no reason to install the
// server-side `clawvisor` CLI.

// daemonHost is the host/port the local daemon listens on for its
// pairing + proxy API. 25299 is the default; override via
// $CLAWVISOR_LOCAL_PORT for dev where the daemon's on a non-default port.
func daemonBaseURL() string {
	port := os.Getenv("CLAWVISOR_LOCAL_PORT")
	if port == "" {
		port = "25299"
	}
	return "http://127.0.0.1:" + port
}

var proxyCmd = &cobra.Command{
	Use:   "proxy",
	Short: "Manage the Clawvisor Network Proxy (observation + credential injection)",
	Long: `Configure, launch, and wrap agents for the Clawvisor Network Proxy.

The proxy is supervised by this daemon — it restarts on crash and
stays alive across reboots. These subcommands talk to the daemon's
local API on 127.0.0.1:25299.

Scoped per-agent: "clawvisor-local proxy run <cmd>" routes only that
one command's traffic through the proxy. Your browser, git, brew,
and everything else stay direct.`,
}

var (
	cfgBinaryPath  string
	cfgServerURL   string
	cfgProxyToken  string
	cfgBridgeID    string
	cfgListenHost  string
	cfgListenPort  int
	cfgMode        string
	cfgAutoEnable  bool
)

var proxyInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Configure the proxy and enable it under the local daemon",
	Long: `Points the daemon at a proxy binary + Clawvisor server + bridge, then
starts the proxy under daemon supervision. Idempotent — rerun with
different args to reconfigure.

Required:
  --binary       path to the clawvisor-proxy executable
  --proxy-token  cvisproxy_… from the dashboard's "Enable Proxy" flow
  --bridge-id    the bridge UUID this proxy serves`,
	SilenceUsage: true,
	RunE:         runProxyInstall,
}

var proxyStatusCmd = &cobra.Command{
	Use:          "status",
	Short:        "Show the daemon-reported proxy state",
	SilenceUsage: true,
	RunE:         runProxyStatus,
}

var proxyEnableCmd = &cobra.Command{
	Use:          "enable",
	Short:        "Start the currently-configured proxy",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemonPOST("/api/proxy/enable", nil)
	},
}

var proxyDisableCmd = &cobra.Command{
	Use:          "disable",
	Short:        "Stop the proxy (stays stopped across daemon restart)",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemonPOST("/api/proxy/disable", nil)
	},
}

var proxyRestartCmd = &cobra.Command{
	Use:          "restart",
	Short:        "Restart the proxy in place (picks up a new binary)",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemonPOST("/api/proxy/restart", nil)
	},
}

var proxySetModeCmd = &cobra.Command{
	Use:          "set-mode <observe|enforce>",
	Short:        "Switch between observe (log only) and enforce (block) modes",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemonPOST("/api/proxy/set-mode", map[string]string{"mode": args[0]})
	},
}

func init() {
	proxyInstallCmd.Flags().StringVar(&cfgBinaryPath, "binary", "",
		"Path to the proxy binary (clawvisor-proxy). Required.")
	proxyInstallCmd.Flags().StringVar(&cfgServerURL, "server-url", "http://127.0.0.1:25297",
		"Clawvisor server URL the proxy should register with.")
	proxyInstallCmd.Flags().StringVar(&cfgProxyToken, "proxy-token", "",
		"cvisproxy_… token minted from the dashboard's 'Enable Proxy' flow. Required.")
	proxyInstallCmd.Flags().StringVar(&cfgBridgeID, "bridge-id", "",
		"Bridge UUID this proxy serves. Required.")
	proxyInstallCmd.Flags().StringVar(&cfgListenHost, "listen-host", "127.0.0.1",
		"Host the proxy should bind to.")
	proxyInstallCmd.Flags().IntVar(&cfgListenPort, "listen-port", 25298,
		"TCP port the proxy should listen on.")
	proxyInstallCmd.Flags().StringVar(&cfgMode, "mode", "observe",
		"observe | enforce. Observe logs decisions; enforce blocks.")
	proxyInstallCmd.Flags().BoolVar(&cfgAutoEnable, "no-start", false,
		"Configure only; don't auto-start the proxy (invert: --no-start=true).")

	proxyCmd.AddCommand(proxyInstallCmd)
	proxyCmd.AddCommand(proxyStatusCmd)
	proxyCmd.AddCommand(proxyEnableCmd)
	proxyCmd.AddCommand(proxyDisableCmd)
	proxyCmd.AddCommand(proxyRestartCmd)
	proxyCmd.AddCommand(proxySetModeCmd)
	proxyCmd.AddCommand(proxyRunCmd)
	proxyCmd.AddCommand(proxyTrustCACmd)
	proxyCmd.AddCommand(proxyUpdateBinaryCmd)
	// Registration with the root command happens in main.go — clawvisor-local
	// constructs its rootCmd locally instead of as a package var.
}

// -- update-binary -------------------------------------------------------

var (
	updateRepo       string
	updateTag        string
	updateForce      bool
	updateFromServer bool
	updateServerURL  string
)

var proxyUpdateBinaryCmd = &cobra.Command{
	Use:   "update-binary",
	Short: "Download the latest proxy binary and restart in place",
	Long: `Downloads a platform-specific clawvisor-proxy binary, installs it
to ~/.clawvisor/proxy/bin/clawvisor-proxy, and reconfigures the
daemon to point at it. The daemon's Configure auto-restarts the
running process so the new binary takes effect immediately.

Sources, in priority order:
  --from-server          Pull from your Clawvisor server's
                         GET /api/proxy/download endpoint. Use during
                         development against a local server that has
                         CLAWVISOR_PROXY_BINARY_DIR set, or against a
                         self-hosted deployment that's serving the
                         binary itself.
  --repo / --tag         GitHub release path. Public repos work
                         unauthenticated; private repos need GH_TOKEN.

When --from-server is set, --repo / --tag are ignored.`,
	SilenceUsage: true,
	RunE:         runProxyUpdateBinary,
}

func init() {
	proxyUpdateBinaryCmd.Flags().StringVar(&updateRepo, "repo", "clawvisor/proxy",
		"GitHub repo (owner/name) to pull releases from.")
	proxyUpdateBinaryCmd.Flags().StringVar(&updateTag, "tag", "latest",
		"Release tag to install ('latest' for the most recent published release).")
	proxyUpdateBinaryCmd.Flags().BoolVar(&updateForce, "force", false,
		"Force download + reinstall even if the current binary's mtime matches.")
	proxyUpdateBinaryCmd.Flags().BoolVar(&updateFromServer, "from-server", false,
		"Pull the binary from the Clawvisor server's /api/proxy/download endpoint "+
			"instead of GitHub. Server URL is taken from --server-url, then "+
			"$CLAWVISOR_SERVER_URL, then the daemon's currently-configured server.")
	proxyUpdateBinaryCmd.Flags().StringVar(&updateServerURL, "server-url", "",
		"Server URL to pull from when --from-server is set. Defaults to "+
			"$CLAWVISOR_SERVER_URL or the daemon's configured server.")
}

type ghAsset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
	Size        int64  `json:"size"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

func runProxyUpdateBinary(cmd *cobra.Command, args []string) error {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binDir := filepath.Join(home, ".clawvisor", "proxy", "bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}
	binPath := filepath.Join(binDir, "clawvisor-proxy")

	if updateFromServer {
		serverURL, err := resolveUpdateServerURL()
		if err != nil {
			return err
		}
		fmt.Printf("Downloading clawvisor-proxy (%s) from %s…\n", platform, serverURL)
		if err := downloadFromServer(serverURL, platform, binPath); err != nil {
			return fmt.Errorf("download from server: %w", err)
		}
	} else {
		// Convention: the proxy release publishes one asset per platform
		// named like "clawvisor-proxy-darwin-arm64",
		// "clawvisor-proxy-linux-amd64", etc.
		wantAsset := "clawvisor-proxy-" + platform
		rel, err := fetchRelease(updateRepo, updateTag)
		if err != nil {
			return fmt.Errorf("fetch release: %w", err)
		}
		var pick *ghAsset
		for i := range rel.Assets {
			if rel.Assets[i].Name == wantAsset || rel.Assets[i].Name == wantAsset+".gz" || rel.Assets[i].Name == wantAsset+".tar.gz" {
				pick = &rel.Assets[i]
				break
			}
		}
		if pick == nil {
			names := make([]string, len(rel.Assets))
			for i, a := range rel.Assets {
				names[i] = a.Name
			}
			return fmt.Errorf("no asset matching %s in release %s. Available: %v", wantAsset, rel.TagName, names)
		}
		fmt.Printf("Downloading %s (%s, %d bytes)…\n", pick.Name, rel.TagName, pick.Size)
		if err := downloadAsset(pick.DownloadURL, binPath); err != nil {
			return fmt.Errorf("download asset: %w", err)
		}
	}

	if err := os.Chmod(binPath, 0755); err != nil {
		return fmt.Errorf("chmod binary: %w", err)
	}
	fmt.Printf("Installed to %s\n", binPath)

	// Reconfigure the daemon to point at the new binary. Configure
	// auto-restarts the running proxy; if it wasn't running we still
	// update the recorded path for next start.
	cur, err := fetchProxyStatusForUpdate()
	if err != nil {
		fmt.Printf("Note: daemon not reachable; binary installed but configure skipped. Re-run 'clawvisor-local proxy install' next.\n")
		return nil
	}
	body := map[string]any{
		"binary_path":  binPath,
		"server_url":   cur.ServerURL,
		"proxy_token":  "", // re-use the stored one
		"bridge_id":    cur.BridgeID,
		"listen_host":  cur.ListenHost,
		"listen_port":  cur.ListenPort,
		"mode":         cur.Mode,
		"auto_enable":  cur.Enabled,
	}
	return daemonPOST("/api/proxy/configure", body)
}

func fetchRelease(repo, tag string) (*ghRelease, error) {
	url := "https://api.github.com/repos/" + repo + "/releases/" + tag
	if tag == "" || tag == "latest" {
		url = "https://api.github.com/repos/" + repo + "/releases/latest"
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GH_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github %d: %s", resp.StatusCode, string(raw))
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// resolveUpdateServerURL picks the server URL for --from-server.
// Priority: --server-url flag, $CLAWVISOR_SERVER_URL, daemon's
// configured server. Errors when none are available so the user knows
// to point us at one explicitly.
func resolveUpdateServerURL() (string, error) {
	if updateServerURL != "" {
		return strings.TrimRight(updateServerURL, "/"), nil
	}
	if v := os.Getenv("CLAWVISOR_SERVER_URL"); v != "" {
		return strings.TrimRight(v, "/"), nil
	}
	if cur, err := fetchProxyStatusForUpdate(); err == nil && cur.ServerURL != "" {
		return strings.TrimRight(cur.ServerURL, "/"), nil
	}
	return "", errors.New("no server URL — pass --server-url, set $CLAWVISOR_SERVER_URL, or run 'clawvisor-local proxy install' first")
}

// downloadFromServer GETs /api/proxy/download from the configured
// Clawvisor server, verifies the X-Clawvisor-Proxy-Sha256 header
// against the bytes we received, and writes them to dst atomically.
func downloadFromServer(serverURL, platform, dst string) error {
	url := serverURL + "/api/proxy/download?platform=" + platform
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server %d: %s", resp.StatusCode, string(raw))
	}
	wantSum := resp.Header.Get("X-Clawvisor-Proxy-Sha256")

	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	gotSum := hex.EncodeToString(hasher.Sum(nil))
	if wantSum != "" && gotSum != wantSum {
		_ = os.Remove(tmp)
		return fmt.Errorf("sha256 mismatch: server announced %s, got %s", wantSum, gotSum)
	}
	return os.Rename(tmp, dst)
}

func downloadAsset(url, dst string) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/octet-stream")
	if tok := os.Getenv("GH_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download %d", resp.StatusCode)
	}
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// fetchProxyStatusForUpdate returns the daemon's current view, used so
// update-binary can preserve the rest of the config (server_url,
// bridge_id, mode) when reconfiguring.
type proxyStatusUpdate struct {
	State      string `json:"state"`
	Enabled    bool   `json:"enabled"`
	ListenHost string `json:"listen_host"`
	ListenPort int    `json:"listen_port"`
	BridgeID   string `json:"bridge_id"`
	ServerURL  string `json:"server_url"`
	Mode       string `json:"mode"`
}

func fetchProxyStatusForUpdate() (*proxyStatusUpdate, error) {
	resp, err := http.Get(daemonBaseURL() + "/api/proxy/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("daemon %d", resp.StatusCode)
	}
	var s proxyStatusUpdate
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	if s.BridgeID == "" {
		return nil, errors.New("no proxy configured yet — run 'clawvisor-local proxy install' first")
	}
	return &s, nil
}

// -- install -------------------------------------------------------------

func runProxyInstall(cmd *cobra.Command, args []string) error {
	if cfgBinaryPath == "" {
		return errors.New("--binary is required (path to the proxy executable)")
	}
	if cfgProxyToken == "" || cfgBridgeID == "" {
		return errors.New("--proxy-token and --bridge-id are required (copy from the dashboard's Enable Proxy flow)")
	}
	abs, err := filepath.Abs(cfgBinaryPath)
	if err != nil {
		return fmt.Errorf("resolve --binary: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("--binary %s: %w", abs, err)
	}
	// Pre-flight: is something already bound to the proxy port? Skip
	// the warning when the daemon's own supervised proxy owns it on
	// the same port — that's the normal "reconfigure live proxy" path
	// and the daemon will Restart it cleanly after persistence.
	if !daemonOwnsPort(cfgListenPort) {
		if owner := portOwner(fmt.Sprintf("127.0.0.1:%d", cfgListenPort)); owner != "" {
			return fmt.Errorf("port %d is already in use (%s). Stop that process or rerun with --listen-port <other>",
				cfgListenPort, owner)
		}
	}

	// Migration: the previous "proxy as a service" install wrote a
	// service manifest that clawvisor-local's ServerManager will try to
	// start. Remove it so the daemon-owned lifecycle is the only path.
	// Safe no-op if it doesn't exist.
	home, _ := os.UserHomeDir()
	legacyDir := filepath.Join(home, ".clawvisor", "local", "services", "clawvisor.network-proxy")
	if _, err := os.Stat(legacyDir); err == nil {
		if err := os.RemoveAll(legacyDir); err != nil {
			return fmt.Errorf("remove legacy service dir %s: %w", legacyDir, err)
		}
		fmt.Printf("Removed legacy service manifest at %s\n", legacyDir)
	}

	body := map[string]any{
		"binary_path":  abs,
		"server_url":   cfgServerURL,
		"proxy_token":  cfgProxyToken,
		"bridge_id":    cfgBridgeID,
		"listen_host":  cfgListenHost,
		"listen_port":  cfgListenPort,
		"mode":         cfgMode,
		"auto_enable":  !cfgAutoEnable, // --no-start inverts default-true
	}
	return daemonPOST("/api/proxy/configure", body)
}

// -- status --------------------------------------------------------------

func runProxyStatus(cmd *cobra.Command, args []string) error {
	resp, err := http.Get(daemonBaseURL() + "/api/proxy/status")
	if err != nil {
		return fmt.Errorf("call daemon: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(raw))
	}
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, raw, "", "  ")
	fmt.Println(pretty.String())
	return nil
}

// -- clawvisor-local proxy run --------------------------------------------------

var (
	runAgentToken string
	runAgentLabel string
	runListenHost string
	runListenPort string
)

var proxyRunCmd = &cobra.Command{
	Use:   "run [flags] -- <command> [args...]",
	Short: "Launch a command with its HTTP traffic scoped through the proxy",
	Long: `Sets HTTP_PROXY, HTTPS_PROXY, and NODE_EXTRA_CA_CERTS only for the
child process, so the rest of your shell is untouched.

Examples:
  # Default: label inferred from the command basename, anonymous attribution.
  clawvisor-local proxy run -- claude-code

  # Tag traffic with an explicit label (multi-agent attribution).
  clawvisor-local proxy run --agent-label researcher -- claude-code

  # Verified-tier enforcement (per-agent bans + policy actually fire).
  # Mint a cvis_ token from the dashboard's Proxies tab first.
  clawvisor-local proxy run --agent-token cvis_abc -- claude-code

  # macOS GUI app (Electron / Chromium). Quit the app first or the
  # proxy flag won't apply.
  clawvisor-local proxy run -- /Applications/Claude.app

Only the invoked command (and its descendants) flow through the proxy.
Your browser, git, brew, etc. are unaffected.`,
	SilenceUsage: true,
	RunE:         runProxyRun,
}

func init() {
	proxyRunCmd.Flags().StringVar(&runAgentToken, "agent-token", "",
		"cvis_… enforcement token. Optional — falls back to $CLAWVISOR_AGENT_TOKEN. "+
			"Required only for verified-tier enforcement (per-agent bans).")
	proxyRunCmd.Flags().StringVar(&runAgentLabel, "agent-label", "",
		"Non-secret label used to attribute traffic in the dashboard "+
			"(e.g. 'claude-code', 'cursor'). Defaults to $CLAWVISOR_AGENT_LABEL.")
	proxyRunCmd.Flags().StringVar(&runListenHost, "host", "127.0.0.1",
		"Proxy host the child process should target.")
	proxyRunCmd.Flags().StringVar(&runListenPort, "port", "25298",
		"Proxy port the child process should target. Default matches 'clawvisor-local proxy install'.")
}

func runProxyRun(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return errors.New("missing command. Example: clawvisor-local proxy run -- claude-code")
	}
	token := runAgentToken
	if token == "" {
		token = os.Getenv("CLAWVISOR_AGENT_TOKEN")
	}
	label := runAgentLabel
	if label == "" {
		label = os.Getenv("CLAWVISOR_AGENT_LABEL")
	}
	// When the user didn't supply a label or a token, derive a label
	// from the command being wrapped (basename of args[0]). Free
	// attribution: `clawvisor-local proxy run -- claude` lands as
	// "claude" in the dashboard instead of "default", and multiple
	// wrapped agents self-distinguish without any flag. Runtime-style
	// invocations (`python script.py`, `node agent.js`) get labeled
	// with the runtime, which is why --agent-label exists as an
	// override.
	if label == "" && token == "" {
		base := filepath.Base(args[0])
		// Strip the .app suffix so attribution shows up as "Claude"
		// rather than "Claude.app".
		label = strings.TrimSuffix(base, ".app")
	}

	// Discover the CA cert path from the daemon's status so users don't
	// have to know the filesystem layout.
	caPath, err := discoverCACertPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(caPath); err != nil {
		return fmt.Errorf("CA cert not found at %s — is the proxy running? Try 'clawvisor-local proxy status': %w", caPath, err)
	}

	// macOS .app bundles are GUI apps that LaunchServices opens via
	// `open`. They don't inherit the env we set on a normal exec call.
	// Dispatch them through a Chromium-flag path that works for
	// Electron apps (Claude Desktop, VS Code, Cursor) — the most
	// common GUI agent shape.
	if runtime.GOOS == "darwin" && strings.HasSuffix(strings.TrimRight(args[0], "/"), ".app") {
		return runProxyRunMacApp(args, runListenHost, runListenPort)
	}

	// Compose Proxy-Authorization basic auth from the label/token pair.
	// The proxy parses (label, token) and derives the attribution tier:
	//   token only       → verified (validated against known agents)
	//   label only       → labeled (advisory; no ban enforcement)
	//   neither          → anonymous (attributed as "default")
	//   label + token    → verified, with a friendlier display label
	proxyURL := buildProxyURL(label, token, runListenHost, runListenPort)

	// Sweep env vars covering as many HTTP clients + trust stores as
	// reasonable. Each line is a different runtime/tool's flavor of the
	// same idea — there's no "standard" beyond HTTP_PROXY and the rest
	// is empirical. Setting all of them costs nothing for clients that
	// ignore them; missing one means that client silently bypasses the
	// proxy.
	env := append(os.Environ(),
		// --- proxy URL: every flavor we know of ---
		"HTTP_PROXY="+proxyURL,            // libcurl, requests, Go net/http, Bun
		"HTTPS_PROXY="+proxyURL,           // same
		"http_proxy="+proxyURL,            // lowercase variants — Linux convention
		"https_proxy="+proxyURL,
		"ALL_PROXY="+proxyURL,             // curl + a number of HTTP libraries
		"all_proxy="+proxyURL,
		"NO_PROXY=localhost,127.0.0.1,::1",
		"no_proxy=localhost,127.0.0.1,::1",

		// --- npm + yarn read these explicitly even when HTTP_PROXY is set ---
		"npm_config_proxy="+proxyURL,
		"npm_config_https_proxy="+proxyURL,
		"NPM_CONFIG_PROXY="+proxyURL,
		"NPM_CONFIG_HTTPS_PROXY="+proxyURL,

		// --- CA trust: every store/library has its own knob ---
		"NODE_EXTRA_CA_CERTS="+caPath,     // Node's built-in https / Bun (also reads this)
		"SSL_CERT_FILE="+caPath,           // OpenSSL (Python urllib, ruby, many others)
		"REQUESTS_CA_BUNDLE="+caPath,      // Python requests
		"CURL_CA_BUNDLE="+caPath,          // curl
		"GIT_SSL_CAINFO="+caPath,          // git
		"AWS_CA_BUNDLE="+caPath,           // AWS SDKs
		"DENO_CERT="+caPath,               // Deno
		"npm_config_cafile="+caPath,       // npm registry TLS verification
		"NPM_CONFIG_CAFILE="+caPath,

		// --- our own marker so child processes can detect they're wrapped ---
		"CLAWVISOR_PROXY="+proxyURL,
		"CLAWVISOR_PROXY_CA="+caPath,
	)

	// Node's built-in fetch() uses undici, which ignores HTTP_PROXY.
	// The shim is preloaded via --require and calls
	// setGlobalDispatcher(new ProxyAgent(...)) so fetch() actually
	// routes through the proxy. Falls through silently in projects
	// where undici can't be resolved (older Node, projects without it
	// as a dep). Doesn't help — and doesn't hurt — non-Node children.
	if shimPath, err := materializeNodeProxyShim(); err == nil {
		env = append(env, "NODE_OPTIONS="+mergeNodeOptions(os.Getenv("NODE_OPTIONS"), "--require="+shimPath))
	} else {
		// Materialize failed (disk full, permissions). Carry on without
		// the shim rather than blocking the child.
		fmt.Fprintf(os.Stderr, "warning: could not install Node fetch shim: %v\n", err)
	}

	c := exec.Command(args[0], args[1:]...) //nolint:gosec
	c.Env = env
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

// runProxyRunMacApp handles the .app dispatch on macOS. Uses
// `open -a` with --args to pass Chromium flags to the wrapped
// Electron app. Quirks worth knowing:
//
//   1. If the app is already running, `open --args` does NOT pass new
//      args to the existing process — they're silently ignored. The
//      caller has to quit the app first. We surface this as a hint.
//
//   2. We can't set Proxy-Authorization basic auth via Chromium's
//      --proxy-server flag — credentials in the URL get stripped.
//      So GUI apps land as anonymous-tier traffic until we wire a
//      separate identity vector (header injection, etc.).
//
//   3. CA trust comes from the macOS keychain, which Chromium reads
//      by default. The user must have run `clawvisor-local proxy
//      trust-ca` once. We don't pass --ignore-certificate-errors;
//      that would disable all TLS verification and silently mask
//      legitimate failures.
//
// Only Electron / Chromium apps benefit. Native macOS apps (Cocoa,
// AppKit) ignore the Chromium flags and continue to make direct
// connections — there's no general-purpose env-var path for them.
func runProxyRunMacApp(args []string, host, port string) error {
	appPath := args[0]
	info, err := os.Stat(appPath)
	if err != nil {
		return fmt.Errorf("app not found: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not an .app bundle", appPath)
	}

	proxyURL := fmt.Sprintf("http://%s:%s", host, port)

	// `open -n` would force a new instance; we DON'T pass it because
	// most users want the single-instance Electron behavior. The
	// downside is the if-already-running quirk — surface it.
	openArgs := []string{"-a", appPath, "--args", "--proxy-server=" + proxyURL}
	openArgs = append(openArgs, args[1:]...) // any extra args after the app path

	c := exec.Command("open", openArgs...) //nolint:gosec
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("open %s: %w", appPath, err)
	}

	fmt.Fprintf(os.Stderr,
		"Launched %s with --proxy-server=%s.\n"+
			"NOTE: if the app was already running, the proxy flag was ignored.\n"+
			"      Quit it (Cmd-Q) and re-run this command.\n"+
			"NOTE: GUI apps land in the dashboard as anonymous-tier — Chromium\n"+
			"      strips basic-auth credentials from --proxy-server URLs.\n",
		filepath.Base(appPath), proxyURL)
	return nil
}

// materializeNodeProxyShim writes the embedded Node fetch-routing
// shim to ~/.clawvisor/local/clawvisor-proxy-shim.js and returns the
// absolute path. Idempotent — only writes when the on-disk content
// differs from the embedded bytes (after binary upgrades).
func materializeNodeProxyShim() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".clawvisor", "local")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "clawvisor-proxy-shim.js")
	if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, nodeProxyShim) {
		return dst, nil
	}
	if err := os.WriteFile(dst, nodeProxyShim, 0644); err != nil {
		return "", err
	}
	return dst, nil
}

// mergeNodeOptions appends one or more Node CLI options to whatever
// the user already had in NODE_OPTIONS. Preserves user options like
// --inspect, --max-old-space-size, etc.
func mergeNodeOptions(existing, addition string) string {
	existing = strings.TrimSpace(existing)
	addition = strings.TrimSpace(addition)
	if existing == "" {
		return addition
	}
	if addition == "" {
		return existing
	}
	return existing + " " + addition
}

// buildProxyURL composes the HTTP_PROXY URL the wrapped child sees.
// Both label and token are optional — see the proxy's
// extractProxyAuthPair for the parsing rules on the receiving side.
func buildProxyURL(label, token, host, port string) string {
	switch {
	case token != "" && label != "":
		return fmt.Sprintf("http://%s:%s@%s:%s", url.QueryEscape(label), url.QueryEscape(token), host, port)
	case token != "":
		// Backcompat shape so older proxies still extract the token from
		// the userinfo when the new pair-aware parser isn't deployed.
		return fmt.Sprintf("http://%s@%s:%s", url.QueryEscape(token), host, port)
	case label != "":
		return fmt.Sprintf("http://%s@%s:%s", url.QueryEscape(label), host, port)
	default:
		return fmt.Sprintf("http://%s:%s", host, port)
	}
}

func discoverCACertPath() (string, error) {
	resp, err := http.Get(daemonBaseURL() + "/api/proxy/status")
	if err == nil && resp.StatusCode == 200 {
		defer resp.Body.Close()
		var s struct {
			CACertPath string `json:"ca_cert_path"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&s); err == nil && s.CACertPath != "" {
			return s.CACertPath, nil
		}
	}
	// Fallback: the conventional path.
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".clawvisor", "proxy-data", "ca.pem"), nil
}

// -- clawvisor-local proxy trust-ca -------------------------------------------

var proxyTrustCACmd = &cobra.Command{
	Use:   "trust-ca",
	Short: "Install the proxy's TLS CA cert into the system trust store",
	Long: `macOS: adds the cert to the user login keychain. Linux: copies to
/usr/local/share/ca-certificates/ and runs update-ca-certificates.

After this, tools that use the system trust store trust the proxy's
MITM certs. Node fetch() still needs NODE_EXTRA_CA_CERTS — 'clawvisor
proxy run' sets that for child processes automatically.`,
	SilenceUsage: true,
	RunE:         runProxyTrustCA,
}

func runProxyTrustCA(cmd *cobra.Command, args []string) error {
	caPath, err := discoverCACertPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(caPath); err != nil {
		return fmt.Errorf("CA cert not found at %s (make sure the proxy has run at least once): %w", caPath, err)
	}

	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		keychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
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

// -- daemon HTTP helpers ------------------------------------------------

func daemonPOST(path string, body interface{}) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	resp, err := http.Post(daemonBaseURL()+path, "application/json", &buf)
	if err != nil {
		return fmt.Errorf("call daemon: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(raw))
	}
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, raw, "", "  ")
	if pretty.Len() > 0 {
		fmt.Println(pretty.String())
	}
	return nil
}

// daemonOwnsPort checks whether the local daemon's supervised proxy
// is bound to the given port. Used to skip the conflict pre-flight
// when the user is reconfiguring a live proxy on the same port — the
// daemon's Restart will release-and-rebind cleanly.
func daemonOwnsPort(port int) bool {
	resp, err := http.Get(daemonBaseURL() + "/api/proxy/status")
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return false
	}
	defer resp.Body.Close()
	var s struct {
		State      string `json:"state"`
		ListenPort int    `json:"listen_port"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return false
	}
	return s.State == "running" && s.ListenPort == port
}

// portOwner returns a short description of what's listening on addr,
// or "" if nothing is. Uses a short dial + lsof to name the owner.
func portOwner(addr string) string {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return ""
	}
	_ = conn.Close()
	parts := splitColon(addr)
	port := parts[len(parts)-1]
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN").Output()
	if err == nil {
		for i, line := range splitLines(string(out)) {
			if i == 0 || line == "" {
				continue
			}
			fields := splitFields(line)
			if len(fields) > 1 {
				return fields[0] + " (PID " + fields[1] + ")"
			}
		}
	}
	return "an existing process"
}

func splitColon(s string) []string { return splitOn(s, ':') }
func splitLines(s string) []string { return splitOn(s, '\n') }

func splitOn(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// splitFields collapses runs of whitespace into single separators.
func splitFields(s string) []string {
	var out []string
	start := -1
	for i := 0; i <= len(s); i++ {
		atEnd := i == len(s)
		isSep := !atEnd && (s[i] == ' ' || s[i] == '\t')
		if atEnd || isSep {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	return out
}
