package isolation

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// HolderInfo identifies a started netns-holder container.
type HolderInfo struct {
	ContainerID string
}

// HolderConfig is the input to StartHolder.
type HolderConfig struct {
	DockerBin    string
	Image        string
	Network      string
	ProxyPort    int
	APIPort      int
	SessionShort string
	// TestAllowPort is an optional port forwarded to the holder as
	// CLAWVISOR_TEST_ALLOW_PORT for integration tests; zero in production.
	// The holder resolves host.docker.internal itself and applies the iptables
	// rule against that resolved IP.
	TestAllowPort int
}

// StartHolder launches the privileged netns-holder container that installs
// iptables rules and idles, holding the netns open for the user container to join.
//
// The holder is given `--add-host host.docker.internal:host-gateway` so its
// init script can resolve the host's IP (works on both Docker Desktop and
// Linux Engine 20.10+). The resolved IP is what the iptables ACCEPT rules
// target, and is also written to /run/clawvisor/host.ip so the launcher can
// read it and use it for the user container's env-var construction.
func StartHolder(ctx context.Context, cfg HolderConfig) (*HolderInfo, error) {
	if cfg.DockerBin == "" || cfg.Image == "" || cfg.Network == "" {
		return nil, fmt.Errorf("holder: docker/image/network required")
	}
	if cfg.ProxyPort <= 0 || cfg.APIPort <= 0 {
		return nil, fmt.Errorf("holder: proxy/api ports required")
	}
	args := []string{
		"run", "-d", "--rm",
		"--network", cfg.Network,
		"--add-host", "host.docker.internal:host-gateway",
		"--cap-add", "NET_ADMIN",
		"--cap-add", "NET_RAW",
		"--label", LabelIsolation,
		"--label", fmt.Sprintf("%s=%d", LabelKeyOwnerPID, os.Getpid()),
		"--label", fmt.Sprintf("%s=%s", LabelKeyCreated, time.Now().UTC().Format(time.RFC3339)),
		"--label", fmt.Sprintf("%s=%s", LabelKeySession, sanitizeShort(cfg.SessionShort)),
		"-e", fmt.Sprintf("CLAWVISOR_PROXY_PORT=%d", cfg.ProxyPort),
		"-e", fmt.Sprintf("CLAWVISOR_API_PORT=%d", cfg.APIPort),
	}
	if cfg.TestAllowPort > 0 {
		args = append(args, "-e", fmt.Sprintf("CLAWVISOR_TEST_ALLOW_PORT=%d", cfg.TestAllowPort))
	}
	args = append(args, cfg.Image, "/usr/local/bin/clawvisor-firewall-and-hold")

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, cfg.DockerBin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker run holder: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	cid := strings.TrimSpace(stdout.String())
	if cid == "" {
		return nil, fmt.Errorf("docker run holder: empty container id")
	}
	if err := waitFirewallReady(ctx, cfg.DockerBin, cid, 10*time.Second); err != nil {
		_ = StopContainer(ctx, cfg.DockerBin, cid)
		return nil, err
	}
	return &HolderInfo{ContainerID: cid}, nil
}

// StopContainer stops a container; ignores errors (container may already be gone).
func StopContainer(ctx context.Context, dockerBin, id string) error {
	if id == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, dockerBin, "stop", "--time", "0", id)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func waitFirewallReady(ctx context.Context, dockerBin, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	delay := 50 * time.Millisecond
	for {
		if execTestFile(ctx, dockerBin, containerID, "/run/clawvisor/firewall.ready") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("holder %s: firewall.ready not present after %s", shortID(containerID), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 500*time.Millisecond {
			delay *= 2
		}
	}
}

func execTestFile(ctx context.Context, dockerBin, containerID, path string) bool {
	cmd := exec.CommandContext(ctx, dockerBin, "exec", containerID, "test", "-f", path)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ParseOwnerPID extracts the owner pid from a container's labels (best-effort).
func ParseOwnerPID(label string) int {
	parts := strings.SplitN(label, "=", 2)
	if len(parts) != 2 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0
	}
	return n
}
