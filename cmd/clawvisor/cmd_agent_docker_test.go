package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

var launchUserPattern = regexp.MustCompile(`^launch-[0-9a-fA-F-]{36}$`)

func TestDeriveContainerURLRewritesLoopbackHost(t *testing.T) {
	got, err := deriveContainerURL("http://127.0.0.1:25297", "host.docker.internal")
	if err != nil {
		t.Fatalf("deriveContainerURL: %v", err)
	}
	if got != "http://host.docker.internal:25297" {
		t.Fatalf("unexpected container URL %q", got)
	}
}

func TestDeriveContainerURLLeavesRemoteHostUnchanged(t *testing.T) {
	got, err := deriveContainerURL("https://clawvisor.example.com", "host.docker.internal")
	if err != nil {
		t.Fatalf("deriveContainerURL: %v", err)
	}
	if got != "https://clawvisor.example.com" {
		t.Fatalf("unexpected container URL %q", got)
	}
}

func TestBuildDockerAgentEnvVars(t *testing.T) {
	opts := &dockerProxyOptions{
		BaseURL:      "http://127.0.0.1:25297",
		ContainerURL: "http://host.docker.internal:25297",
		AgentToken:   "cvis_test_token",
		ProxyHost:    "host.docker.internal",
		ProxyPort:    25290,
		CAInside:     "/clawvisor/ca.pem",
		CAHost:       "/host/ca.pem",
	}

	values := map[string]string{}
	for _, v := range buildDockerAgentEnvVars(opts, false) {
		values[v.Key] = v.Value
	}

	if got := values["CLAWVISOR_URL"]; got != "http://host.docker.internal:25297" {
		t.Fatalf("unexpected CLAWVISOR_URL %q", got)
	}
	httpProxy := values["HTTP_PROXY"]
	user, suffix := splitProxyUserAndSuffix(t, httpProxy)
	if !launchUserPattern.MatchString(user) {
		t.Fatalf("expected launch-<uuid> user prefix, got %q in %q", user, httpProxy)
	}
	if suffix != "cvis_test_token@host.docker.internal:25290" {
		t.Fatalf("unexpected HTTP_PROXY suffix %q", suffix)
	}
	if got := values["CLAWVISOR_RUNTIME_CA_CERT_FILE"]; got != "/clawvisor/ca.pem" {
		t.Fatalf("unexpected CA path %q", got)
	}
	if got := values["OPENCLAW_PROXY_ACTIVE"]; got != "1" {
		t.Fatalf("unexpected OPENCLAW_PROXY_ACTIVE %q", got)
	}
	if got := values["NO_PROXY"]; got != "localhost,127.0.0.1,::1,host.docker.internal" {
		t.Fatalf("unexpected NO_PROXY %q", got)
	}
}

func splitProxyUserAndSuffix(t *testing.T, raw string) (string, string) {
	t.Helper()
	const prefix = "http://"
	if !strings.HasPrefix(raw, prefix) {
		t.Fatalf("expected http:// scheme, got %q", raw)
	}
	rest := raw[len(prefix):]
	user, after, ok := strings.Cut(rest, ":")
	if !ok {
		t.Fatalf("missing user:secret separator in %q", raw)
	}
	return user, after
}

func TestBuildDockerAgentEnvVarsAssignsUniqueLaunchID(t *testing.T) {
	opts := &dockerProxyOptions{
		AgentToken: "cvis_test_token",
		ProxyHost:  "host.docker.internal",
		ProxyPort:  25290,
		CAInside:   "/clawvisor/ca.pem",
	}
	first := proxyURLFromVars(buildDockerAgentEnvVars(opts, false))
	second := proxyURLFromVars(buildDockerAgentEnvVars(opts, false))
	firstUser, _ := splitProxyUserAndSuffix(t, first)
	secondUser, _ := splitProxyUserAndSuffix(t, second)
	if firstUser == secondUser {
		t.Fatalf("expected unique launch ids per invocation, got %q twice", firstUser)
	}
}

func proxyURLFromVars(vars []dockerEnvVar) string {
	for _, v := range vars {
		if v.Key == "HTTP_PROXY" {
			return v.Value
		}
	}
	return ""
}

func TestBuildDockerAgentEnvVarsTemplated(t *testing.T) {
	opts := &dockerProxyOptions{
		ContainerURL: "http://host.docker.internal:25297",
		AgentToken:   "ignored",
		ProxyHost:    "host.docker.internal",
		ProxyPort:    25290,
		CAInside:     "/clawvisor/ca.pem",
	}
	values := map[string]string{}
	for _, v := range buildDockerAgentEnvVars(opts, true) {
		values[v.Key] = v.Value
	}
	if got := values["CLAWVISOR_AGENT_TOKEN"]; got != "${CLAWVISOR_AGENT_TOKEN}" {
		t.Fatalf("unexpected templated agent token %q", got)
	}
	if got := values["HTTP_PROXY"]; !strings.Contains(got, "${CLAWVISOR_AGENT_TOKEN}") {
		t.Fatalf("expected templated token in HTTP_PROXY, got %q", got)
	}
	if got := values["HTTP_PROXY"]; !strings.Contains(got, "launch-") {
		t.Fatalf("expected launch-<uuid> user in HTTP_PROXY, got %q", got)
	}
	if _, ok := values["CLAWVISOR_RUNTIME_SESSION_ID"]; ok {
		t.Fatalf("durable docker env should not pre-mint runtime session ids, got %+v", values)
	}
	if strings.Contains(values["HTTP_PROXY"], "runtime-secret") {
		t.Fatalf("durable docker env should not embed runtime session secrets, got %q", values["HTTP_PROXY"])
	}
}

func TestBuildDockerRunInjection(t *testing.T) {
	injected := buildDockerRunInjection([]dockerEnvVar{
		{Key: "HTTP_PROXY", Value: "http://proxy"},
		{Key: "SSL_CERT_FILE", Value: "/clawvisor/ca.pem"},
	}, "/host/ca.pem", "/clawvisor/ca.pem", "host.docker.internal")
	got := strings.Join(injected, " ")
	if !strings.Contains(got, "--add-host host.docker.internal:host-gateway") {
		t.Fatalf("expected host gateway alias, got %q", got)
	}
	if !strings.Contains(got, "-v /host/ca.pem:/clawvisor/ca.pem:ro") {
		t.Fatalf("expected CA mount, got %q", got)
	}
	if !strings.Contains(got, "-e HTTP_PROXY=http://proxy") {
		t.Fatalf("expected HTTP_PROXY env, got %q", got)
	}
}

func TestFindDockerRunImageIndex(t *testing.T) {
	idx, err := findDockerRunImageIndex([]string{"--rm", "-it", "--name", "agent", "-v", "/tmp:/tmp", "my-image", "run"})
	if err != nil {
		t.Fatalf("findDockerRunImageIndex: %v", err)
	}
	if idx != 6 {
		t.Fatalf("unexpected image index %d", idx)
	}
}

func TestEmitDockerComposeOverrideTemplated(t *testing.T) {
	opts := &dockerProxyOptions{
		ContainerURL: "http://host.docker.internal:25297",
		AgentToken:   "ignored",
		ProxyHost:    "host.docker.internal",
		ProxyPort:    25290,
		CAInside:     "/clawvisor/ca.pem",
		CAHost:       "/host/ca.pem",
	}
	var buf bytes.Buffer
	emitDockerComposeOverride(&buf, dockerComposeOverrideOptions{
		Service:      "agent",
		Opts:         opts,
		Templated:    true,
		EnvVars:      buildDockerAgentEnvVars(opts, true),
		ProxyHost:    opts.ProxyHost,
		ContainerURL: opts.ContainerURL,
	})
	out := buf.String()
	if !strings.Contains(out, `CLAWVISOR_AGENT_TOKEN: "${CLAWVISOR_AGENT_TOKEN}"`) {
		t.Fatalf("expected templated agent token in compose override, got:\n%s", out)
	}
	matchedHTTPProxy := regexp.MustCompile(`HTTP_PROXY: "http://launch-[0-9a-fA-F-]{36}:\$\{CLAWVISOR_AGENT_TOKEN\}@host\.docker\.internal:25290"`).MatchString(out)
	if !matchedHTTPProxy {
		t.Fatalf("expected templated HTTP_PROXY with launch-<uuid> in compose override, got:\n%s", out)
	}
	if !strings.Contains(out, `- "/host/ca.pem:/clawvisor/ca.pem:ro"`) {
		t.Fatalf("expected CA mount in compose override, got:\n%s", out)
	}
}

func TestPrintDockerEnvAsArgsUsesShellQuoting(t *testing.T) {
	var buf bytes.Buffer
	printDockerEnvAsArgs(&buf, []dockerEnvVar{
		{Key: "TOKEN", Value: `value with spaces $HOME ! backtick` + "`" + ` and 'quote'`},
	})
	got := strings.TrimSpace(buf.String())
	want := "-e 'TOKEN=value with spaces $HOME ! backtick` and '\\''quote'\\'''"
	if got != want {
		t.Fatalf("unexpected docker args output:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildIsolatedDockerRunInjection(t *testing.T) {
	injected := buildIsolatedDockerRunInjection(
		[]dockerEnvVar{
			{Key: "CLAWVISOR_URL", Value: "http://172.20.0.1:34567"},
			{Key: "HTTPS_PROXY", Value: "http://launch-x:tok@172.20.0.1:34568"},
		},
		"/host/ca.pem", "/clawvisor/ca.pem",
		"holderID0123",
		[]string{"clawvisor.company.internal:172.20.0.1"},
	)
	got := strings.Join(injected, " ")
	if !strings.Contains(got, "--network container:holderID0123") {
		t.Fatalf("expected --network container:<holder>, got %q", got)
	}
	if !strings.Contains(got, "--add-host clawvisor.company.internal:172.20.0.1") {
		t.Fatalf("expected --add-host for preserved hostname, got %q", got)
	}
	if !strings.Contains(got, "-v /host/ca.pem:/clawvisor/ca.pem:ro") {
		t.Fatalf("expected CA mount, got %q", got)
	}
	if !strings.Contains(got, "-e CLAWVISOR_URL=http://172.20.0.1:34567") {
		t.Fatalf("expected CLAWVISOR_URL env, got %q", got)
	}
}

func TestBuildIsolatedDockerRunInjectionNoHostnameAddsNoExtraHosts(t *testing.T) {
	injected := buildIsolatedDockerRunInjection(
		[]dockerEnvVar{{Key: "CLAWVISOR_URL", Value: "http://172.20.0.1:34567"}},
		"/host/ca.pem", "/clawvisor/ca.pem",
		"holderID0123",
		nil,
	)
	got := strings.Join(injected, " ")
	if strings.Contains(got, "--add-host") {
		t.Fatalf("did not expect --add-host for loopback rewrite, got %q", got)
	}
}

func TestAppendNoProxyHostsMergesIntoBothCases(t *testing.T) {
	vars := []dockerEnvVar{
		{Key: "NO_PROXY", Value: "localhost,127.0.0.1"},
		{Key: "no_proxy", Value: "localhost,127.0.0.1"},
		{Key: "HTTP_PROXY", Value: "http://x"},
	}
	out := appendNoProxyHosts(vars, "clawvisor.company.internal", "172.20.0.1")
	wantSubs := []string{"clawvisor.company.internal", "172.20.0.1"}
	for _, v := range out {
		if v.Key != "NO_PROXY" && v.Key != "no_proxy" {
			continue
		}
		for _, s := range wantSubs {
			if !strings.Contains(v.Value, s) {
				t.Fatalf("%s: missing %q in %q", v.Key, s, v.Value)
			}
		}
	}
}

func TestRuntimeProxyUpstreamAddrUnboundLoopback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".clawvisor")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(`
runtime_proxy:
  listen_addr: "0.0.0.0:25290"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}
	if got := runtimeProxyUpstreamAddr(0); got != "127.0.0.1:25290" {
		t.Fatalf("runtimeProxyUpstreamAddr(0) = %q, want 127.0.0.1:25290", got)
	}
	if got := runtimeProxyUpstreamAddr(31337); got != "127.0.0.1:31337" {
		t.Fatalf("runtimeProxyUpstreamAddr(31337) = %q, want 127.0.0.1:31337 (--proxy-port override)", got)
	}
}

func TestRuntimeProxyUpstreamAddrCustomHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".clawvisor")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(`
runtime_proxy:
  listen_addr: "10.0.0.5:25290"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}
	if got := runtimeProxyUpstreamAddr(0); got != "10.0.0.5:25290" {
		t.Fatalf("runtimeProxyUpstreamAddr(0) = %q, want 10.0.0.5:25290 (preserve config host)", got)
	}
	if got := runtimeProxyUpstreamAddr(31337); got != "10.0.0.5:31337" {
		t.Fatalf("runtimeProxyUpstreamAddr(31337) = %q, want 10.0.0.5:31337 (host from config, port override)", got)
	}
}

func TestDockerDefaultsFollowLocalRuntimeConfig(t *testing.T) {
	home := t.TempDir()
	prevHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("Setenv(HOME): %v", err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("HOME", prevHome)
	})

	cfgDir := filepath.Join(home, ".clawvisor")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	caDir := filepath.Join(home, "runtime-ca")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(`
runtime_proxy:
  listen_addr: "127.0.0.1:4318"
  data_dir: "`+caDir+`"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}

	if got := defaultRuntimeProxyPort(); got != 4318 {
		t.Fatalf("defaultRuntimeProxyPort() = %d, want 4318", got)
	}
	if got := defaultRuntimeProxyCAHostPath(); got != filepath.Join(caDir, "ca.pem") {
		t.Fatalf("defaultRuntimeProxyCAHostPath() = %q, want %q", got, filepath.Join(caDir, "ca.pem"))
	}
}

func TestDefaultRuntimeProxyCAHostPathFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	prevHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("Setenv(HOME): %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("HOME", prevHome) })

	// No config file → configured path resolves to ~/.clawvisor/runtime-proxy/ca.pem
	// already (the home-relative default). To make the fallback observable, we
	// drop a CA file at the home path and verify it's selected even when the
	// configured DataDir points elsewhere.
	cfgDir := filepath.Join(home, ".clawvisor")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	bogusDataDir := filepath.Join(home, "does-not-exist")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(`
runtime_proxy:
  data_dir: "`+bogusDataDir+`"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}

	homeCAPath := filepath.Join(home, ".clawvisor", "runtime-proxy", "ca.pem")
	if err := os.MkdirAll(filepath.Dir(homeCAPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(home CA dir): %v", err)
	}
	if err := os.WriteFile(homeCAPath, []byte("home"), 0o644); err != nil {
		t.Fatalf("WriteFile(home CA): %v", err)
	}

	if got := defaultRuntimeProxyCAHostPath(); got != homeCAPath {
		t.Fatalf("defaultRuntimeProxyCAHostPath() = %q, want %q (home fallback)", got, homeCAPath)
	}
}

func TestDefaultRuntimeProxyCAHostPathFallsBackToWorkspace(t *testing.T) {
	home := t.TempDir()
	prevHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("Setenv(HOME): %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("HOME", prevHome) })

	wd := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(wd); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	// Workspace-relative path mimics a daemon launched with a relative
	// DataDir from this directory; verify the CLI finds it.
	resolvedWD, err := filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	wsCAPath := filepath.Join(resolvedWD, ".clawvisor", "runtime-proxy", "ca.pem")
	if err := os.MkdirAll(filepath.Dir(wsCAPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(wsCAPath, []byte("ws"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if got := defaultRuntimeProxyCAHostPath(); got != wsCAPath {
		t.Fatalf("defaultRuntimeProxyCAHostPath() = %q, want %q (workspace fallback)", got, wsCAPath)
	}
}

// TestWaitWithSignalRelayForwardsSIGTERM proves that a SIGTERM delivered to
// the parent process is relayed to the child rather than killing it via
// context cancellation. The child traps SIGTERM and exits 42; if the relay
// were broken (e.g. exec.CommandContext SIGKILL on ctx-cancel) we'd observe
// a different exit code or no exit code at all.
func TestWaitWithSignalRelayForwardsSIGTERM(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	child := exec.Command("bash", "-c", `trap "exit 42" TERM; sleep 30 & wait`)
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		// Give bash a beat to install its trap before signaling.
		time.Sleep(100 * time.Millisecond)
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}()

	err := waitWithSignalRelay(ctx, child)
	if err == nil {
		t.Fatal("expected non-nil error (child should have exited 42)")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if got := exitErr.ExitCode(); got != 42 {
		t.Fatalf("child exit code = %d, want 42 (signal relay → trapped TERM)", got)
	}
}

// TestWaitWithSignalRelayContextCancelTerminatesChild proves that ctx
// cancellation flows through as SIGTERM to the child (not SIGKILL) so the
// child can run its shutdown trap.
func TestWaitWithSignalRelayContextCancelTerminatesChild(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	child := exec.Command("bash", "-c", `trap "exit 17" TERM; sleep 30 & wait`)
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	err := waitWithSignalRelay(ctx, child)
	if err == nil {
		t.Fatal("expected non-nil error (child should have exited 17)")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if got := exitErr.ExitCode(); got != 17 {
		t.Fatalf("child exit code = %d, want 17 (ctx cancel → SIGTERM relay)", got)
	}
}

// TestWaitWithSignalRelayNaturalExit confirms the happy path: child exits on
// its own, helper returns its error.
func TestWaitWithSignalRelayNaturalExit(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skipf("/bin/true not available: %v", err)
	}
	child := exec.Command("true")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	if err := waitWithSignalRelay(context.Background(), child); err != nil {
		t.Fatalf("expected nil error from successful child, got %v", err)
	}
}

// runDockerComposeIsolated end-to-end coverage: missing flags, then a
// minimal happy path that emits the holder + user service stanzas.

func withResetDockerComposeIsolationFlags(t *testing.T) {
	t.Helper()
	prev := struct {
		svc, exposeURL, exposeAPIURL, isolation string
		tpl                                     bool
	}{dockerComposeSvc, dockerComposeExposeURL, dockerComposeExposeAPIURL, dockerIsolation, dockerComposeTpl}
	t.Cleanup(func() {
		dockerComposeSvc = prev.svc
		dockerComposeExposeURL = prev.exposeURL
		dockerComposeExposeAPIURL = prev.exposeAPIURL
		dockerIsolation = prev.isolation
		dockerComposeTpl = prev.tpl
	})
}

func TestRunDockerComposeIsolatedRequiresExposeFlags(t *testing.T) {
	withResetDockerComposeIsolationFlags(t)
	dockerComposeSvc = "agent"
	dockerComposeExposeURL = ""
	dockerComposeExposeAPIURL = "http://192.168.1.10:18791"

	opts := &dockerProxyOptions{
		ContainerURL: "http://host.docker.internal:25297",
		AgentToken:   "tok",
		ProxyHost:    "host.docker.internal",
		ProxyPort:    25290,
		CAInside:     "/clawvisor/ca.pem",
		CAHost:       "/host/ca.pem",
	}
	var buf bytes.Buffer
	if err := runDockerComposeIsolated(&buf, opts); err == nil {
		t.Fatal("expected error when --expose-url is missing")
	}
}

func TestRunDockerComposeIsolatedEmitsHolderAndUserService(t *testing.T) {
	withResetDockerComposeIsolationFlags(t)
	dockerComposeSvc = "agent"
	dockerComposeExposeURL = "http://192.168.1.10:25291"
	dockerComposeExposeAPIURL = "http://192.168.1.10:18791"
	dockerComposeTpl = true

	opts := &dockerProxyOptions{
		ContainerURL: "http://192.168.1.10:18791",
		AgentToken:   "tok",
		ProxyHost:    "host.docker.internal",
		ProxyPort:    25290,
		CAInside:     "/clawvisor/ca.pem",
		CAHost:       "/host/ca.pem",
	}
	var buf bytes.Buffer
	if err := runDockerComposeIsolated(&buf, opts); err != nil {
		t.Fatalf("runDockerComposeIsolated: %v", err)
	}
	out := buf.String()

	wants := []string{
		"  clawvisor-netns-holder:",
		"      - NET_ADMIN",
		`      CLAWVISOR_HOST_TARGET: "192.168.1.10"`,
		`      CLAWVISOR_PROXY_PORT: "25291"`,
		`      CLAWVISOR_API_PORT: "18791"`,
		"  agent:",
		`    network_mode: "service:clawvisor-netns-holder"`,
		`        condition: service_healthy`,
		`      CLAWVISOR_URL: "http://192.168.1.10:18791"`,
		// proxy plumbing is rewritten to the expose host:port; templated token preserved.
		`@192.168.1.10:25291`,
	}
	for _, s := range wants {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q\n--- output ---\n%s", s, out)
		}
	}
	if !strings.Contains(out, "192.168.1.10") {
		t.Fatalf("NO_PROXY should include the API host so traffic to CLAWVISOR_URL bypasses the proxy. Output:\n%s", out)
	}
}
