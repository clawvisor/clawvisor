// Package isolation provides iptables-locked egress for clawvisor-launched
// docker containers. The user container shares a network namespace with a
// privileged "holder" sidecar; the holder installs a default-deny iptables
// policy that only permits TCP to two host-side forwarders (runtime proxy +
// daemon API), guaranteeing that any direct connect() from the workload
// returns ECONNREFUSED regardless of the userspace HTTP client's behavior.
package isolation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// Mode identifies an isolation mode for `clawvisor agent docker-run`.
type Mode string

const (
	ModeOff       Mode = "off"
	ModeContainer Mode = "container"
)

// ParseMode validates a string flag value.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeOff:
		return ModeOff, nil
	case ModeContainer:
		return ModeContainer, nil
	default:
		return "", fmt.Errorf("unknown isolation mode %q (want off or container)", s)
	}
}

// Plan is the input to Prepare.
type Plan struct {
	// DockerBin is the path to the docker CLI.
	DockerBin string
	// BaseURL is the daemon API endpoint the user container will speak to
	// (creds.BaseURL — local loopback or remote). Used to route the API forwarder.
	BaseURL string
	// UpstreamProxyAddr is host:port of the runtime proxy on the host.
	UpstreamProxyAddr string
	// SessionShort is a short, human-friendly identifier mixed into network/labels.
	SessionShort string
	// TestAllowHostPort is an optional `IP:PORT` for integration tests; empty in production.
	TestAllowHostPort string
}

// Handle is the result of a successful Prepare. Cleanup must be called when
// the user container exits to release the network, holder, and forwarders.
type Handle struct {
	plan      Plan
	network   *NetworkInfo
	holder    *HolderInfo
	hostIP    string
	proxyFwd  *Forwarder
	apiFwd    *Forwarder
	testFwd   *Forwarder
	rewritten *RewrittenURL

	cleanupOnce sync.Once
	cleanupErr  error
	cleaned     atomic.Bool
}

// forwarderBindAddr is the address the host-side forwarders bind on.
// 0.0.0.0 is the right choice on macOS Docker Desktop (where bridge gateway
// IPs live inside the VM and are unreachable from the host process) and works
// fine on Linux Engine too — the in-netns workload still only sees the bridge
// gateway IP, never a host-routable address.
const forwarderBindAddr = "0.0.0.0:0"

// Prepare provisions a fresh isolated execution context: it builds the holder
// image (cached after first build), creates a labeled bridge network, starts
// host-side forwarders for the proxy and API, then launches the netns-holder
// container and waits for its iptables rules to be in place. The returned
// Handle exposes the holder container ID and the forwarder ports the user
// container's env vars should reference.
func Prepare(ctx context.Context, plan Plan) (handle *Handle, err error) {
	if plan.DockerBin == "" {
		return nil, errors.New("isolation: DockerBin required")
	}
	if plan.BaseURL == "" {
		return nil, errors.New("isolation: BaseURL required")
	}
	if plan.UpstreamProxyAddr == "" {
		return nil, errors.New("isolation: UpstreamProxyAddr required")
	}

	apiUpstream, err := ResolveUpstream(plan.BaseURL)
	if err != nil {
		return nil, err
	}

	PruneStale(ctx, plan.DockerBin)

	image, err := EnsureImage(ctx, plan.DockerBin)
	if err != nil {
		return nil, err
	}

	network, err := CreateNetwork(ctx, plan.DockerBin, plan.SessionShort)
	if err != nil {
		return nil, err
	}

	h := &Handle{plan: plan, network: network}
	defer func() {
		if err != nil {
			_ = h.Cleanup()
		}
	}()

	proxyFwd, err := StartForwarder(ctx, forwarderBindAddr, plan.UpstreamProxyAddr)
	if err != nil {
		return nil, fmt.Errorf("start proxy forwarder: %w", err)
	}
	h.proxyFwd = proxyFwd

	apiFwd, err := StartForwarder(ctx, forwarderBindAddr, apiUpstream)
	if err != nil {
		return nil, fmt.Errorf("start api forwarder: %w", err)
	}
	h.apiFwd = apiFwd

	if plan.TestAllowHostPort != "" {
		testFwd, err := StartForwarder(ctx, forwarderBindAddr, plan.TestAllowHostPort)
		if err != nil {
			return nil, fmt.Errorf("start test forwarder: %w", err)
		}
		h.testFwd = testFwd
	}

	holder, err := StartHolder(ctx, HolderConfig{
		DockerBin:     plan.DockerBin,
		Image:         image,
		Network:       network.Name,
		ProxyPort:     proxyFwd.Port(),
		APIPort:       apiFwd.Port(),
		SessionShort:  plan.SessionShort,
		TestAllowPort: testForwarderPort(h.testFwd),
	})
	if err != nil {
		return nil, err
	}
	h.holder = holder

	hostIP, err := readHolderHostIP(ctx, plan.DockerBin, holder.ContainerID)
	if err != nil {
		return nil, err
	}
	h.hostIP = hostIP

	rewritten, err := RewriteAPIURL(plan.BaseURL, hostIP, apiFwd.Port())
	if err != nil {
		return nil, err
	}
	h.rewritten = rewritten

	return h, nil
}

func testForwarderPort(testFwd *Forwarder) int {
	if testFwd == nil {
		return 0
	}
	return testFwd.Port()
}

// readHolderHostIP reads the host IP that the holder resolved for
// host.docker.internal. The holder writes it to /run/clawvisor/host.ip during
// firewall init; the user container will reach the host-side forwarders at
// this IP.
func readHolderHostIP(ctx context.Context, dockerBin, containerID string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, dockerBin, "exec", containerID, "cat", "/run/clawvisor/host.ip")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("read host.ip from holder: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	ip := strings.TrimSpace(stdout.String())
	if ip == "" {
		return "", fmt.Errorf("holder host.ip is empty")
	}
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("holder host.ip %q is not a valid IP", ip)
	}
	return ip, nil
}

// HolderContainerID is the running holder container's ID; the user container
// joins this netns via `--network=container:<id>`.
func (h *Handle) HolderContainerID() string { return h.holder.ContainerID }

// GatewayIP is the host IP the user container uses to reach the host-side
// forwarders. Use this as the proxy host in HTTPS_PROXY.
//
// On Docker Desktop the bridge gateway IP lives inside the VM and is
// unreachable from host-bound forwarders; instead we use whatever the holder
// resolved for host.docker.internal, which routes correctly on both Docker
// Desktop and Linux Engine 20.10+ (with `--add-host host.docker.internal:host-gateway`).
func (h *Handle) GatewayIP() string { return h.hostIP }

// ProxyForwarderPort is the port the runtime-proxy forwarder is bound on.
func (h *Handle) ProxyForwarderPort() int { return h.proxyFwd.Port() }

// APIForwarderPort is the port the daemon-API forwarder is bound on.
func (h *Handle) APIForwarderPort() int { return h.apiFwd.Port() }

// ContainerAPIURL is the rewritten CLAWVISOR_URL the user container should use.
func (h *Handle) ContainerAPIURL() string { return h.rewritten.URL }

// ExtraAddHosts returns any `--add-host` entries the user container needs so
// that the rewritten CLAWVISOR_URL hostname resolves to the host-side forwarder.
// Returns nil for loopback / IP-literal base URLs.
func (h *Handle) ExtraAddHosts() []string {
	if h.rewritten.Kind != HostKindDNSName {
		return nil
	}
	return []string{fmt.Sprintf("%s:%s", h.rewritten.Hostname, h.hostIP)}
}

// PreservedHostname is the DNS hostname embedded in CLAWVISOR_URL after rewrite,
// or empty if the rewrite uses the gateway IP directly.
func (h *Handle) PreservedHostname() string {
	if h.rewritten.Kind != HostKindDNSName {
		return ""
	}
	return h.rewritten.Hostname
}

// NetworkName returns the bridge network created for this invocation.
func (h *Handle) NetworkName() string { return h.network.Name }
