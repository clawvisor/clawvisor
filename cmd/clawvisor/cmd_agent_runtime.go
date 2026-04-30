package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	runtimeproxy "github.com/clawvisor/clawvisor/internal/runtime/proxy"
	"github.com/clawvisor/clawvisor/internal/tui/client"
	"github.com/spf13/cobra"
)

var (
	runtimeAgentName  string
	runtimeAgentToken string
	runtimeServerURL  string
	runtimeMode       string
	runtimeTTLSeconds int
	runtimeObserve    bool
)

var agentRuntimeEnvCmd = &cobra.Command{
	Use:   "runtime-env",
	Short: "Print proxy environment exports for a runtime-scoped agent run",
	Long:  "Mint a short-lived runtime session for an agent token and print shell exports that route the child process through Clawvisor's embedded runtime proxy.",
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := runtimeBootstrapOptionsFromFlags(cmd)
		if err != nil {
			return err
		}
		session, err := createRuntimeBootstrapSession(opts)
		if err != nil {
			return err
		}
		envPairs, err := buildRuntimeBootstrapEnv(opts.BaseURL, opts.AgentToken, session)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Runtime session %s active until %s\n", session.Session.ID, session.Session.ExpiresAt.Format("2006-01-02 15:04:05 MST"))
		printObserveModeNotice(session.ObservationMode)
		for _, pair := range envPairs {
			key, value, _ := strings.Cut(pair, "=")
			fmt.Printf("export %s=%s\n", key, shellQuote(value))
		}
		return nil
	},
	SilenceUsage: true,
}

var agentRuntimeRunCmd = &cobra.Command{
	Use:   "run -- <command> [args...]",
	Short: "Run a command inside a runtime-scoped proxy session",
	Long:  "Mint a short-lived runtime session for an agent token, inject the required proxy environment, and exec the given command under that session.",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := runtimeBootstrapOptionsFromFlags(cmd)
		if err != nil {
			return err
		}
		_ = maybeOfferStarterProfile(opts.Credentials, args)
		session, err := createRuntimeBootstrapSession(opts)
		if err != nil {
			return err
		}
		envPairs, err := buildRuntimeBootstrapEnv(opts.BaseURL, opts.AgentToken, session)
		if err != nil {
			return err
		}
		child := exec.Command(args[0], args[1:]...)
		child.Env = mergeEnvironment(os.Environ(), envPairs)
		child.Stdin = os.Stdin
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		fmt.Fprintf(os.Stderr, "Runtime session %s active until %s\n", session.Session.ID, session.Session.ExpiresAt.Format("2006-01-02 15:04:05 MST"))
		printObserveModeNotice(session.ObservationMode)
		return child.Run()
	},
	SilenceUsage: true,
}

type runtimeBootstrapOptions struct {
	Credentials *resolvedAgentCredentials
	BaseURL     string
	AgentToken  string
	Mode        string
	TTLSeconds  int
	Observation *bool
	Profile     string
}

func runtimeBootstrapOptionsFromFlags(cmd *cobra.Command) (*runtimeBootstrapOptions, error) {
	creds, err := resolveAgentCredentials(runtimeAgentName, runtimeAgentToken, runtimeServerURL)
	if err != nil {
		return nil, err
	}
	var observation *bool
	if cmd != nil && cmd.Flags().Changed("observe") {
		observe := runtimeObserve
		observation = &observe
	}
	return &runtimeBootstrapOptions{
		Credentials: creds,
		BaseURL:     creds.BaseURL,
		AgentToken:  creds.AgentToken,
		Mode:        runtimeMode,
		TTLSeconds:  runtimeTTLSeconds,
		Observation: observation,
		Profile:     strings.TrimSpace(strings.ToLower(runtimeProfileOverride)),
	}, nil
}

func createRuntimeBootstrapSession(opts *runtimeBootstrapOptions) (*client.CreateRuntimeSessionResponse, error) {
	cl := client.New(opts.BaseURL, "")
	cl.SetAccessToken(opts.AgentToken)
	metadata := runtimeBootstrapMetadata()
	metadata["launcher"] = "clawvisor-agent-run"
	if opts.Profile != "" {
		metadata["starter_profile"] = opts.Profile
	}
	return cl.CreateRuntimeSession(client.CreateRuntimeSessionRequest{
		Mode:            opts.Mode,
		ObservationMode: opts.Observation,
		TTLSeconds:      opts.TTLSeconds,
		Metadata:        metadata,
	})
}

func runtimeBootstrapMetadata() map[string]any {
	metadata := map[string]any{}
	if wd, err := os.Getwd(); err == nil {
		metadata["working_dir"] = wd
		if roots := defaultToolAllowedRoots(wd); len(roots) > 0 {
			metadata["tool_allowed_roots"] = roots
		}
	}
	return metadata
}

func defaultToolAllowedRoots(workingDir string) []string {
	seen := map[string]bool{}
	roots := make([]string, 0, 3)
	add := func(root string) {
		root = strings.TrimSpace(root)
		if root == "" {
			return
		}
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		root = filepath.Clean(root)
		if root == "." || seen[root] {
			return
		}
		seen[root] = true
		roots = append(roots, root)
	}
	add(workingDir)
	add("/tmp")
	add(os.TempDir())
	return roots
}

func buildRuntimeBootstrapEnv(baseURL, agentToken string, session *client.CreateRuntimeSessionResponse) ([]string, error) {
	if session == nil {
		return nil, fmt.Errorf("runtime session response is required")
	}
	authenticatedProxyURL, err := runtimeproxy.ProxyURLWithSecret(session.ProxyURL, session.ProxyBearer)
	if err != nil {
		return nil, err
	}
	noProxy := mergeNoProxy(os.Getenv("NO_PROXY"), "127.0.0.1", "localhost", "::1")
	envPairs := []string{
		"CLAWVISOR_URL=" + baseURL,
		"CLAWVISOR_AGENT_TOKEN=" + agentToken,
		"CLAWVISOR_RUNTIME_SESSION_ID=" + session.Session.ID,
		"CLAWVISOR_RUNTIME_PROXY_URL=" + session.ProxyURL,
		"CLAWVISOR_RUNTIME_OBSERVATION_MODE=" + fmt.Sprintf("%t", session.ObservationMode),
		"HTTP_PROXY=" + authenticatedProxyURL,
		"HTTPS_PROXY=" + authenticatedProxyURL,
		"ALL_PROXY=" + authenticatedProxyURL,
		"http_proxy=" + authenticatedProxyURL,
		"https_proxy=" + authenticatedProxyURL,
		"all_proxy=" + authenticatedProxyURL,
		"NO_PROXY=" + noProxy,
		"no_proxy=" + noProxy,
	}
	if strings.TrimSpace(session.CACertPEM) == "" {
		return envPairs, nil
	}
	caPath, err := writeRuntimeCACertFile(session.Session.ID, session.CACertPEM)
	if err != nil {
		return nil, err
	}
	envPairs = append(envPairs,
		"CLAWVISOR_RUNTIME_CA_CERT_FILE="+caPath,
		"SSL_CERT_FILE="+caPath,
		"CURL_CA_BUNDLE="+caPath,
		"REQUESTS_CA_BUNDLE="+caPath,
		"NODE_EXTRA_CA_CERTS="+caPath,
		"GIT_SSL_CAINFO="+caPath,
	)
	return envPairs, nil
}

func writeRuntimeCACertFile(sessionID, pem string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("runtime session id is required for CA cert export")
	}
	if strings.TrimSpace(pem) == "" {
		return "", fmt.Errorf("runtime CA cert PEM is required")
	}
	dir := filepath.Join(os.TempDir(), "clawvisor-runtime-ca")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create runtime CA dir: %w", err)
	}
	cleanupRuntimeCACertFiles(dir, 24*time.Hour)
	path := filepath.Join(dir, sessionID+".pem")
	if err := os.WriteFile(path, []byte(pem), 0o600); err != nil {
		return "", fmt.Errorf("write runtime CA cert: %w", err)
	}
	return path, nil
}

func cleanupRuntimeCACertFiles(dir string, maxAge time.Duration) {
	if strings.TrimSpace(dir) == "" || maxAge <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pem") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

func mergeNoProxy(existing string, defaults ...string) string {
	seen := map[string]bool{}
	var values []string
	for _, raw := range strings.Split(existing, ",") {
		value := strings.TrimSpace(raw)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		values = append(values, value)
	}
	for _, value := range defaults {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		values = append(values, value)
	}
	return strings.Join(values, ",")
}

func mergeEnvironment(base []string, overrides []string) []string {
	byKey := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, exists := byKey[key]; !exists {
			order = append(order, key)
		}
		byKey[key] = entry
	}
	for _, entry := range overrides {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, exists := byKey[key]; !exists {
			order = append(order, key)
		}
		byKey[key] = entry
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func init() {
	for _, subcmd := range []*cobra.Command{agentRuntimeEnvCmd, agentRuntimeRunCmd} {
		subcmd.Flags().StringVar(&runtimeAgentName, "agent", "", "Registered agent name (see `clawvisor agent register`)")
		subcmd.Flags().StringVar(&runtimeAgentToken, "agent-token", "", "Agent bearer token (defaults to CLAWVISOR_AGENT_TOKEN)")
		subcmd.Flags().StringVar(&runtimeServerURL, "url", "", "Clawvisor server URL (overrides the registered agent URL, otherwise defaults to CLAWVISOR_URL or http://127.0.0.1:25297)")
		subcmd.Flags().StringVar(&runtimeMode, "mode", "proxy", "Runtime session mode")
		subcmd.Flags().IntVar(&runtimeTTLSeconds, "ttl-seconds", 0, "Runtime session TTL in seconds (default: server runtime_proxy.session_ttl_seconds)")
		subcmd.Flags().BoolVar(&runtimeObserve, "observe", false, "Create the runtime session in observation mode")
		subcmd.Flags().StringVar(&runtimeProfileOverride, "runtime-profile", "", "Explicit starter profile hint for this launch (e.g. claude_code or codex)")
		subcmd.MarkFlagsMutuallyExclusive("agent", "agent-token")
	}
	agentCmd.AddCommand(agentRuntimeEnvCmd)
	agentCmd.AddCommand(agentRuntimeRunCmd)
}
