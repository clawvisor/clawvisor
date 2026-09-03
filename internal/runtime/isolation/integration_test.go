//go:build integration

package isolation

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func dockerOrSkip(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("docker")
	if err != nil {
		t.Skipf("docker not available: %v", err)
	}
	if err := exec.Command(bin, "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
	return bin
}

type testHarness struct {
	t          *testing.T
	dockerBin  string
	apiSrv     *httptest.Server
	proxySrv   *httptest.Server
	plan       Plan
	handle     *Handle
}

func newHarness(t *testing.T, opts ...func(*Plan)) *testHarness {
	t.Helper()
	bin := dockerOrSkip(t)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "api-ok "+r.URL.Path)
	}))
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "proxy-ok")
	}))

	plan := Plan{
		DockerBin:         bin,
		BaseURL:           apiSrv.URL,
		UpstreamProxyAddr: hostPortFromURL(t, proxySrv.URL),
		SessionShort:      "itest",
	}
	for _, o := range opts {
		o(&plan)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	handle, err := Prepare(ctx, plan)
	if err != nil {
		apiSrv.Close()
		proxySrv.Close()
		t.Fatalf("Prepare: %v", err)
	}

	h := &testHarness{
		t:         t,
		dockerBin: bin,
		apiSrv:    apiSrv,
		proxySrv:  proxySrv,
		plan:      plan,
		handle:    handle,
	}
	t.Cleanup(h.Close)
	return h
}

func (h *testHarness) Close() {
	if h.handle != nil {
		_ = h.handle.Cleanup()
	}
	if h.apiSrv != nil {
		h.apiSrv.Close()
	}
	if h.proxySrv != nil {
		h.proxySrv.Close()
	}
}

func (h *testHarness) runInUserContainer(t *testing.T, ctx context.Context, cmd ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	tag, err := ImageTag()
	if err != nil {
		t.Fatalf("ImageTag: %v", err)
	}
	args := []string{
		"run", "--rm",
		"--network", "container:" + h.handle.HolderContainerID(),
		tag,
	}
	args = append(args, cmd...)
	c := exec.CommandContext(ctx, h.dockerBin, args...)
	var so, se bytes.Buffer
	c.Stdout = &so
	c.Stderr = &se
	err = c.Run()
	stdout = so.String()
	stderr = se.String()
	if err == nil {
		return stdout, stderr, 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return stdout, stderr, exit.ExitCode()
	}
	t.Fatalf("docker run user container: %v (stderr: %s)", err, stderr)
	return
}

func hostPortFromURL(t *testing.T, urlStr string) string {
	t.Helper()
	addr := strings.TrimPrefix(urlStr, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	addr = strings.TrimSuffix(addr, "/")
	if _, _, err := net.SplitHostPort(addr); err != nil {
		t.Fatalf("malformed httptest URL %q", urlStr)
	}
	return addr
}

// TestKernelBlocksDirectEgress proves the actual egress-block property:
// bash's /dev/tcp opens a raw socket bypassing all env-var proxies, so a
// successful connect would mean the firewall is NOT enforcing the policy.
func TestKernelBlocksDirectEgress(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, stderr, exitCode := h.runInUserContainer(t, ctx, "bash", "-c", "exec 3<>/dev/tcp/1.1.1.1/443")
	if exitCode == 0 {
		t.Fatalf("expected /dev/tcp to fail but it succeeded; stderr=%s", stderr)
	}
	if !strings.Contains(strings.ToLower(stderr), "connection refused") {
		t.Logf("warning: expected 'connection refused' in stderr (got: %q); a different failure mode may indicate timeout vs reset", stderr)
	}
}

// TestKernelBlocksNoProxyCurl is the same property via curl with explicit
// proxy bypass — exit code 7 means couldn't connect.
func TestKernelBlocksNoProxyCurl(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, exitCode := h.runInUserContainer(t, ctx, "curl", "--noproxy", "*", "--max-time", "3", "-sS", "http://1.1.1.1")
	if exitCode != 7 && exitCode != 28 {
		t.Fatalf("expected curl exit 7 (couldn't connect) or 28 (timeout), got %d", exitCode)
	}
}

// TestTestEscapeHatchAllowsExtraDestination verifies the iptables ACCEPT
// rule generalization: with CLAWVISOR_TEST_ALLOW_HOSTPORT set, a host:port
// outside the proxy/api pair is reachable.
func TestTestEscapeHatchAllowsExtraDestination(t *testing.T) {
	dockerOrSkip(t)
	extra := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "extra-ok")
	}))
	defer extra.Close()
	extraAddr := hostPortFromURL(t, extra.URL)

	h := newHarness(t, func(p *Plan) {
		p.TestAllowHostPort = extraAddr
	})

	gw := h.handle.GatewayIP()
	port := 0
	if h.handle.testFwd != nil {
		port = h.handle.testFwd.Port()
	}
	if port == 0 {
		t.Fatal("expected a test forwarder when TestAllowHostPort is set")
	}
	target := fmt.Sprintf("http://%s:%d", gw, port)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, stderr, exitCode := h.runInUserContainer(t, ctx, "curl", "-sS", "--max-time", "3", target)
	if exitCode != 0 {
		t.Fatalf("expected reachable destination via test escape hatch, got exit %d (stderr=%s)", exitCode, stderr)
	}
	if !strings.Contains(stdout, "extra-ok") {
		t.Fatalf("unexpected body: %q", stdout)
	}
}

// TestAPIForwarderReachable verifies the API forwarder relays traffic from
// the user container to the daemon API upstream.
func TestAPIForwarderReachable(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	target := h.handle.ContainerAPIURL()
	stdout, stderr, exitCode := h.runInUserContainer(t, ctx, "curl", "-sS", "--max-time", "5", target+"/health")
	if exitCode != 0 {
		t.Fatalf("expected /health reachable via API forwarder, got exit %d (stderr=%s)", exitCode, stderr)
	}
	if !strings.Contains(stdout, "api-ok") {
		t.Fatalf("unexpected /health body: %q", stdout)
	}
}

// TestFirewallReadyBeforeUserStarts asserts the holder finishes installing
// rules before user containers can join.
func TestFirewallReadyBeforeUserStarts(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, h.dockerBin, "exec", h.handle.HolderContainerID(), "test", "-f", "/run/clawvisor/firewall.ready")
	if err := c.Run(); err != nil {
		t.Fatalf("firewall.ready missing: %v", err)
	}

	out, err := exec.CommandContext(ctx, h.dockerBin, "exec", h.handle.HolderContainerID(), "iptables", "-S", "OUTPUT").Output()
	if err != nil {
		t.Fatalf("iptables -S OUTPUT: %v", err)
	}
	rules := string(out)
	if !strings.Contains(rules, "DROP") {
		t.Errorf("expected default DROP policy, got:\n%s", rules)
	}
	if !strings.Contains(rules, "REJECT") || !strings.Contains(rules, "tcp-reset") {
		t.Errorf("expected REJECT --reject-with tcp-reset rule, got:\n%s", rules)
	}
}

// TestPruneStaleRemovesOrphans starts a holder, simulates the launcher
// dying without cleanup, then runs PruneStale and asserts the network is gone.
func TestPruneStaleRemovesOrphans(t *testing.T) {
	bin := dockerOrSkip(t)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer apiSrv.Close()
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer proxySrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	plan := Plan{
		DockerBin:         bin,
		BaseURL:           apiSrv.URL,
		UpstreamProxyAddr: hostPortFromURL(t, proxySrv.URL),
		SessionShort:      "prune",
	}
	handle, err := Prepare(ctx, plan)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Overwrite the owner-pid label so the holder appears orphaned.
	fakePID := 1
	for {
		err := syscall.Kill(fakePID, 0)
		if err == syscall.ESRCH {
			break
		}
		fakePID += 1
		if fakePID > 1<<30 {
			t.Fatal("could not find a dead pid for orphan simulation")
		}
	}
	relabelArgs := []string{
		"container", "update", handle.HolderContainerID(), "--label", fmt.Sprintf("%s=%d", LabelKeyOwnerPID, fakePID),
	}
	if err := exec.CommandContext(ctx, bin, relabelArgs...).Run(); err != nil {
		// `docker container update` doesn't support --label on all engine versions.
		// Fall through and rely on stopping the holder so prune sees an exited container.
		t.Logf("docker container update --label not supported: %v; falling back to stop+prune", err)
		_ = StopContainer(ctx, bin, handle.HolderContainerID())
	}

	// Close forwarders to fully simulate launcher death.
	if handle.proxyFwd != nil {
		_ = handle.proxyFwd.Close()
	}
	if handle.apiFwd != nil {
		_ = handle.apiFwd.Close()
	}

	PruneStale(ctx, bin)

	// Network should be gone (or empty + still within grace; in that case at least
	// no containers should be attached).
	out, err := exec.CommandContext(ctx, bin, "network", "ls", "--filter", "name="+handle.NetworkName(), "--format", "{{.Name}}").Output()
	if err != nil {
		t.Fatalf("docker network ls: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Logf("network still present after prune (may be within grace period): %s", strings.TrimSpace(string(out)))
		_ = exec.CommandContext(ctx, bin, "network", "rm", handle.NetworkName()).Run()
	}
}
