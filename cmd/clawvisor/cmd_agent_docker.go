package main

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/pkg/config"
)

var (
	dockerContainerURL string
	dockerProxyHost    string
	dockerProxyPort    int
	dockerCAInside     string
	dockerCAHost       string
	dockerEnvFormat    string
	dockerEnvQuiet     bool
	dockerRunDryRun    bool
	dockerComposeSvc   string
	dockerComposeTpl   bool
)

type dockerProxyOptions struct {
	Credentials  *resolvedAgentCredentials
	BaseURL      string
	ContainerURL string
	AgentToken   string
	ProxyHost    string
	ProxyPort    int
	CAInside     string
	CAHost       string
}

type dockerEnvVar struct {
	Key     string
	Value   string
	Comment string
}

var agentDockerEnvCmd = &cobra.Command{
	Use:   "docker-env",
	Short: "Print env vars for a Dockerized agent using durable proxy auth",
	Long:  "Print container-ready environment variables for a Dockerized agent. Uses the long-lived agent token for proxy authentication and avoids baking short-lived runtime session secrets into container config.",
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := dockerProxyOptionsFromFlags()
		if err != nil {
			return err
		}
		vars := buildDockerAgentEnvVars(opts, false)
		if !dockerEnvQuiet {
			printDockerEnvHeader(os.Stdout, opts)
		}
		switch dockerEnvFormat {
		case "env":
			printDockerEnvAsEnv(os.Stdout, vars)
		case "export":
			printDockerEnvAsExport(os.Stdout, vars)
		case "docker-args":
			printDockerEnvAsArgs(os.Stdout, vars)
		default:
			return fmt.Errorf("unknown --format %q (want env | export | docker-args)", dockerEnvFormat)
		}
		return nil
	},
	SilenceUsage: true,
}

var agentDockerRunCmd = &cobra.Command{
	Use:                   "docker-run [flags] -- docker run [args...]",
	Short:                 "Run a Dockerized agent with Clawvisor proxy plumbing injected",
	DisableFlagsInUseLine: true,
	Long: `Wrap a docker run invocation with the environment variables and CA mount
needed for a Dockerized agent to route traffic through Clawvisor's embedded
runtime proxy using a long-lived agent token.

Example:

  clawvisor agent docker-run --agent-token "$CLAWVISOR_AGENT_TOKEN" -- \
    docker run --rm -it my-agent-image agent serve
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := dockerProxyOptionsFromFlags()
		if err != nil {
			return err
		}
		if len(args) < 2 || args[0] != "docker" || args[1] != "run" {
			return fmt.Errorf("expected `-- docker run ...` after the flags; got %v", args)
		}
		imageIdx, err := findDockerRunImageIndex(args[2:])
		if err != nil {
			return fmt.Errorf("parse docker run args: %w", err)
		}
		imageIdx += 2
		if imageIdx+1 < len(args) {
			_ = maybeOfferStarterProfile(opts.Credentials, args[imageIdx+1:])
		}
		injected := buildDockerRunInjection(buildDockerAgentEnvVars(opts, false), opts.CAHost, opts.CAInside, opts.ProxyHost)
		final := make([]string, 0, len(args)+len(injected))
		final = append(final, args[:imageIdx]...)
		final = append(final, injected...)
		final = append(final, args[imageIdx:]...)
		if dockerRunDryRun {
			fmt.Println(formatCmdLine(final))
			return nil
		}
		path, err := exec.LookPath("docker")
		if err != nil {
			return fmt.Errorf("docker not found on PATH: %w", err)
		}
		return syscall.Exec(path, final, os.Environ())
	},
	SilenceUsage: true,
}

var agentDockerComposeCmd = &cobra.Command{
	Use:   "docker-compose",
	Short: "Emit a Compose override wiring a service through the runtime proxy",
	Long: `Emit a docker-compose override file for a named service. The generated
override uses the durable agent token path so containers can restart without
requiring a freshly minted runtime session secret.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := dockerProxyOptionsFromFlags()
		if err != nil {
			return err
		}
		if strings.TrimSpace(dockerComposeSvc) == "" {
			return fmt.Errorf("--service is required")
		}
		emitDockerComposeOverride(os.Stdout, dockerComposeOverrideOptions{
			Service:      dockerComposeSvc,
			Opts:         opts,
			Templated:    dockerComposeTpl,
			EnvVars:      buildDockerAgentEnvVars(opts, dockerComposeTpl),
			ProxyHost:    opts.ProxyHost,
			ContainerURL: opts.ContainerURL,
		})
		return nil
	},
	SilenceUsage: true,
}

type dockerComposeOverrideOptions struct {
	Service      string
	Opts         *dockerProxyOptions
	Templated    bool
	EnvVars      []dockerEnvVar
	ProxyHost    string
	ContainerURL string
}

func dockerProxyOptionsFromFlags() (*dockerProxyOptions, error) {
	creds, err := resolveAgentCredentials(runtimeAgentName, runtimeAgentToken, runtimeServerURL)
	if err != nil {
		return nil, err
	}
	containerURL := strings.TrimSpace(dockerContainerURL)
	if containerURL == "" {
		containerURL, err = deriveContainerURL(creds.BaseURL, dockerProxyHost)
		if err != nil {
			return nil, err
		}
	}
	proxyPort := dockerProxyPort
	if proxyPort <= 0 {
		proxyPort = defaultRuntimeProxyPort()
	}
	caHost := strings.TrimSpace(dockerCAHost)
	if caHost == "" {
		caHost = defaultRuntimeProxyCAHostPath()
	}
	return &dockerProxyOptions{
		Credentials:  creds,
		BaseURL:      creds.BaseURL,
		ContainerURL: containerURL,
		AgentToken:   creds.AgentToken,
		ProxyHost:    strings.TrimSpace(dockerProxyHost),
		ProxyPort:    proxyPort,
		CAInside:     strings.TrimSpace(dockerCAInside),
		CAHost:       caHost,
	}, nil
}

func buildDockerAgentEnvVars(opts *dockerProxyOptions, templated bool) []dockerEnvVar {
	token := opts.AgentToken
	if templated {
		token = "${CLAWVISOR_AGENT_TOKEN}"
	}
	authenticatedProxyURL := fmt.Sprintf("http://clawvisor:%s@%s:%d", token, opts.ProxyHost, opts.ProxyPort)
	noProxy := mergeNoProxy("", "localhost", "127.0.0.1", "::1", opts.ProxyHost)
	proxyURL := fmt.Sprintf("http://%s:%d", opts.ProxyHost, opts.ProxyPort)
	return []dockerEnvVar{
		{Key: "CLAWVISOR_URL", Value: opts.ContainerURL, Comment: "Clawvisor API URL the container should use"},
		{Key: "CLAWVISOR_AGENT_TOKEN", Value: token, Comment: "Long-lived agent token for gateway/task APIs and proxy auth"},
		{Key: "CLAWVISOR_RUNTIME_PROXY_URL", Value: proxyURL, Comment: "Runtime proxy base URL without embedded credentials"},
		{Key: "CLAWVISOR_RUNTIME_PROXY_AUTH_MODE", Value: "agent_token", Comment: "Proxy auth mode for durable container launches"},
		{Key: "CLAWVISOR_RUNTIME_CA_CERT_FILE", Value: opts.CAInside, Comment: "Mounted runtime proxy CA certificate path inside the container"},
		{Key: "HTTP_PROXY", Value: authenticatedProxyURL, Comment: "HTTP proxy URL authenticated with the agent token"},
		{Key: "HTTPS_PROXY", Value: authenticatedProxyURL, Comment: "HTTPS proxy URL authenticated with the agent token"},
		{Key: "ALL_PROXY", Value: authenticatedProxyURL, Comment: "Fallback proxy URL for libraries that honor ALL_PROXY"},
		{Key: "http_proxy", Value: authenticatedProxyURL, Comment: ""},
		{Key: "https_proxy", Value: authenticatedProxyURL, Comment: ""},
		{Key: "all_proxy", Value: authenticatedProxyURL, Comment: ""},
		{Key: "NO_PROXY", Value: noProxy, Comment: "Bypass the proxy for Clawvisor itself and container loopback"},
		{Key: "no_proxy", Value: noProxy, Comment: ""},
		{Key: "SSL_CERT_FILE", Value: opts.CAInside, Comment: "CA trust for Go/OpenSSL-linked clients"},
		{Key: "CURL_CA_BUNDLE", Value: opts.CAInside, Comment: "CA trust for curl/libcurl clients"},
		{Key: "REQUESTS_CA_BUNDLE", Value: opts.CAInside, Comment: "CA trust for Python requests"},
		{Key: "NODE_EXTRA_CA_CERTS", Value: opts.CAInside, Comment: "CA trust for Node.js TLS"},
		{Key: "GIT_SSL_CAINFO", Value: opts.CAInside, Comment: "CA trust for git over HTTPS"},
	}
}

func deriveContainerURL(baseURL, proxyHost string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse --url: %w", err)
	}
	host := parsed.Hostname()
	if host == "" {
		return "", fmt.Errorf("base URL %q is missing a hostname", baseURL)
	}
	if !isLoopbackHostname(host) {
		return parsed.String(), nil
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	parsed.Host = net.JoinHostPort(proxyHost, port)
	return parsed.String(), nil
}

func isLoopbackHostname(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	default:
		return false
	}
}

func defaultRuntimeProxyCAHostPath() string {
	cfg := loadLocalDockerRuntimeConfig()
	dir := expandConfigPath(cfg.RuntimeProxy.DataDir)
	return filepath.Join(dir, "ca.pem")
}

func expandHomePath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func expandConfigPath(path string) string {
	path = expandHomePath(path)
	if filepath.IsAbs(path) {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func loadLocalDockerRuntimeConfig() *config.Config {
	cfg := config.Default()
	home, err := os.UserHomeDir()
	if err != nil {
		return cfg
	}
	localCfg, err := config.Load(filepath.Join(home, ".clawvisor", "config.yaml"))
	if err != nil || localCfg == nil {
		return cfg
	}
	return localCfg
}

func defaultRuntimeProxyPort() int {
	cfg := loadLocalDockerRuntimeConfig()
	if _, port, err := net.SplitHostPort(strings.TrimSpace(cfg.RuntimeProxy.ListenAddr)); err == nil {
		if n, convErr := strconv.Atoi(port); convErr == nil && n > 0 {
			return n
		}
	}
	return 25290
}

func printDockerEnvHeader(w io.Writer, opts *dockerProxyOptions) {
	fmt.Fprintln(w, "# Clawvisor Docker env")
	fmt.Fprintln(w, "#")
	fmt.Fprintf(w, "# Container Clawvisor URL: %s\n", opts.ContainerURL)
	fmt.Fprintf(w, "# Runtime proxy:          http://%s:%d\n", opts.ProxyHost, opts.ProxyPort)
	fmt.Fprintf(w, "# Mount runtime proxy CA: %s:%s:ro\n", opts.CAHost, opts.CAInside)
	fmt.Fprintln(w)
}

func printDockerEnvAsEnv(w io.Writer, vars []dockerEnvVar) {
	for _, v := range vars {
		if v.Comment != "" {
			fmt.Fprintf(w, "# %s\n", v.Comment)
		}
		fmt.Fprintf(w, "%s=%s\n", v.Key, v.Value)
	}
}

func printDockerEnvAsExport(w io.Writer, vars []dockerEnvVar) {
	for _, v := range vars {
		if v.Comment != "" {
			fmt.Fprintf(w, "# %s\n", v.Comment)
		}
		fmt.Fprintf(w, "export %s=%s\n", v.Key, shellQuote(v.Value))
	}
}

func printDockerEnvAsArgs(w io.Writer, vars []dockerEnvVar) {
	for i, v := range vars {
		if i > 0 {
			fmt.Fprint(w, " ")
		}
		fmt.Fprintf(w, "-e %s", shellQuote(v.Key+"="+v.Value))
	}
	fmt.Fprintln(w)
}

func buildDockerRunInjection(vars []dockerEnvVar, caHost, caInside, proxyHost string) []string {
	out := make([]string, 0, len(vars)*2+4)
	if strings.Contains(proxyHost, "host.docker.internal") {
		out = append(out, "--add-host", "host.docker.internal:host-gateway")
	}
	out = append(out, "-v", fmt.Sprintf("%s:%s:ro", caHost, caInside))
	for _, v := range vars {
		out = append(out, "-e", v.Key+"="+v.Value)
	}
	return out
}

func findDockerRunImageIndex(args []string) (int, error) {
	skipNext := false
	for i, tok := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if tok == "--" {
			if i+1 >= len(args) {
				return 0, fmt.Errorf("`--` with no image after it")
			}
			return i + 1, nil
		}
		if strings.HasPrefix(tok, "--") {
			if strings.Contains(tok, "=") || isDockerBoolLong(tok) {
				continue
			}
			skipNext = true
			continue
		}
		if strings.HasPrefix(tok, "-") && len(tok) > 1 {
			if isDockerBoolShortRun(tok) {
				continue
			}
			skipNext = true
			continue
		}
		return i, nil
	}
	return 0, fmt.Errorf("no image name found in docker run args")
}

func isDockerBoolLong(flag string) bool {
	switch flag {
	case "--detach", "--interactive", "--tty", "--rm", "--init", "--privileged",
		"--read-only", "--publish-all", "--no-healthcheck", "--quiet",
		"--disable-content-trust", "--oom-kill-disable", "--sig-proxy":
		return true
	}
	return false
}

func isDockerBoolShortRun(tok string) bool {
	if !strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "--") {
		return false
	}
	for _, ch := range tok[1:] {
		switch ch {
		case 'd', 'i', 't', 'P', 'q':
		default:
			return false
		}
	}
	return true
}

func formatCmdLine(argv []string) string {
	var b strings.Builder
	for i, arg := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		if needsShellQuote(arg) {
			b.WriteByte('\'')
			b.WriteString(strings.ReplaceAll(arg, "'", `'\''`))
			b.WriteByte('\'')
		} else {
			b.WriteString(arg)
		}
	}
	return b.String()
}

func needsShellQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("._/:=@+-", r):
		default:
			return true
		}
	}
	return false
}

func emitDockerComposeOverride(w io.Writer, opts dockerComposeOverrideOptions) {
	fmt.Fprintf(w, "# clawvisor agent docker-compose override for service=%q\n", opts.Service)
	fmt.Fprintln(w, "#")
	fmt.Fprintln(w, "# This override uses durable agent-token proxy auth. The container will")
	fmt.Fprintln(w, "# route egress through Clawvisor without requiring a pre-minted runtime")
	fmt.Fprintln(w, "# session secret in the Compose file.")
	if opts.Templated {
		fmt.Fprintln(w, "#")
		fmt.Fprintln(w, "# Export the agent token before running compose:")
		fmt.Fprintln(w, "#   export CLAWVISOR_AGENT_TOKEN=<agent token>")
	}
	fmt.Fprintln(w, "#")
	fmt.Fprintf(w, "# Mount runtime proxy CA from host: %s\n", opts.Opts.CAHost)
	fmt.Fprintf(w, "# Container Clawvisor URL: %s\n", opts.ContainerURL)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "services:")
	fmt.Fprintf(w, "  %s:\n", opts.Service)
	if strings.Contains(opts.ProxyHost, "host.docker.internal") {
		fmt.Fprintln(w, "    extra_hosts:")
		fmt.Fprintln(w, `      - "host.docker.internal:host-gateway"`)
	}
	fmt.Fprintln(w, "    environment:")
	keyed := make(map[string]dockerEnvVar, len(opts.EnvVars))
	keys := make([]string, 0, len(opts.EnvVars))
	for _, v := range opts.EnvVars {
		keyed[v.Key] = v
		keys = append(keys, v.Key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := keyed[key]
		if v.Comment != "" {
			fmt.Fprintf(w, "      # %s\n", v.Comment)
		}
		fmt.Fprintf(w, "      %s: %s\n", v.Key, yamlQuote(v.Value))
	}
	fmt.Fprintln(w, "    volumes:")
	fmt.Fprintf(w, "      - %s\n", yamlQuote(fmt.Sprintf("%s:%s:ro", opts.Opts.CAHost, opts.Opts.CAInside)))
}

func yamlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func init() {
	for _, subcmd := range []*cobra.Command{agentDockerEnvCmd, agentDockerRunCmd, agentDockerComposeCmd} {
		subcmd.Flags().StringVar(&runtimeAgentName, "agent", "", "Registered agent name (see `clawvisor agent register`)")
		subcmd.Flags().StringVar(&runtimeAgentToken, "agent-token", "", "Agent bearer token (defaults to CLAWVISOR_AGENT_TOKEN)")
		subcmd.Flags().StringVar(&runtimeServerURL, "url", "", "Clawvisor server URL the agent should use (overrides the registered agent URL, otherwise defaults to CLAWVISOR_URL or http://127.0.0.1:25297)")
		subcmd.Flags().StringVar(&dockerContainerURL, "container-url", "", "Clawvisor server URL as seen from inside the container (defaults to a container-safe rewrite of --url)")
		subcmd.Flags().StringVar(&dockerProxyHost, "proxy-host", "host.docker.internal", "Hostname the container uses to reach the runtime proxy")
		subcmd.Flags().IntVar(&dockerProxyPort, "proxy-port", 0, "Port the runtime proxy listens on (defaults to the local runtime proxy config)")
		subcmd.Flags().StringVar(&dockerCAInside, "ca-path", "/clawvisor/ca.pem", "Path the runtime proxy CA will be mounted at inside the container")
		subcmd.Flags().StringVar(&dockerCAHost, "ca-host-path", "", "Path to the runtime proxy CA on the host (default: ~/.clawvisor/runtime-proxy/ca.pem)")
		subcmd.Flags().StringVar(&runtimeProfileOverride, "runtime-profile", "", "Explicit starter profile hint for this launch (e.g. claude_code or codex)")
		subcmd.MarkFlagsMutuallyExclusive("agent", "agent-token")
	}
	agentDockerEnvCmd.Flags().StringVar(&dockerEnvFormat, "format", "env", "Output format: env, export, or docker-args")
	agentDockerEnvCmd.Flags().BoolVar(&dockerEnvQuiet, "quiet", false, "Suppress the instructional header")
	agentDockerRunCmd.Flags().BoolVar(&dockerRunDryRun, "dry-run", false, "Print the modified docker command without executing")
	agentDockerComposeCmd.Flags().StringVar(&dockerComposeSvc, "service", "", "Compose service name to wire through Clawvisor (required)")
	agentDockerComposeCmd.Flags().BoolVar(&dockerComposeTpl, "templated", true, "Emit ${CLAWVISOR_AGENT_TOKEN} references instead of baking the token into the override")

	agentCmd.AddCommand(agentDockerEnvCmd)
	agentCmd.AddCommand(agentDockerRunCmd)
	agentCmd.AddCommand(agentDockerComposeCmd)
}
