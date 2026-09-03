package isolation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Network labels used to identify clawvisor-managed networks for pruning + GC.
const (
	LabelIsolation  = "clawvisor.isolation=1"
	LabelKeyOwnerPID = "clawvisor.owner-pid"
	LabelKeyCreated  = "clawvisor.created-at"
	LabelKeySession  = "clawvisor.session"
)

// NetworkInfo describes a freshly-created bridge network for one isolation invocation.
type NetworkInfo struct {
	Name      string
	GatewayIP string
}

// CreateNetwork creates a labeled bridge network and returns its name + bridge gateway IP.
// sessionShort is mixed into the network name for easy debugging; an additional random
// suffix avoids collisions across concurrent invocations.
func CreateNetwork(ctx context.Context, dockerBin, sessionShort string) (*NetworkInfo, error) {
	suffix, err := randomSuffix(2)
	if err != nil {
		return nil, err
	}
	short := sanitizeShort(sessionShort)
	if short == "" {
		short = "iso"
	}
	name := fmt.Sprintf("clawvisor-iso-%s-%s", short, suffix)

	created := time.Now().UTC().Format(time.RFC3339)
	args := []string{
		"network", "create",
		"--driver", "bridge",
		"--label", LabelIsolation,
		"--label", fmt.Sprintf("%s=%d", LabelKeyOwnerPID, os.Getpid()),
		"--label", fmt.Sprintf("%s=%s", LabelKeyCreated, created),
		"--label", fmt.Sprintf("%s=%s", LabelKeySession, short),
		name,
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker network create %s: %w (stderr: %s)", name, err, strings.TrimSpace(stderr.String()))
	}

	gw, err := inspectGateway(ctx, dockerBin, name)
	if err != nil {
		_ = removeNetwork(ctx, dockerBin, name)
		return nil, err
	}
	return &NetworkInfo{Name: name, GatewayIP: gw}, nil
}

// RemoveNetwork removes a network created by CreateNetwork. Idempotent.
func RemoveNetwork(ctx context.Context, dockerBin, name string) error {
	return removeNetwork(ctx, dockerBin, name)
}

func removeNetwork(ctx context.Context, dockerBin, name string) error {
	if name == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, dockerBin, "network", "rm", name)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

type networkInspect struct {
	IPAM struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

func inspectGateway(ctx context.Context, dockerBin, name string) (string, error) {
	out, err := exec.CommandContext(ctx, dockerBin, "network", "inspect", name).Output()
	if err != nil {
		return "", fmt.Errorf("docker network inspect %s: %w", name, err)
	}
	var nets []networkInspect
	if err := json.Unmarshal(out, &nets); err != nil {
		return "", fmt.Errorf("parse network inspect: %w", err)
	}
	for _, n := range nets {
		for _, c := range n.IPAM.Config {
			if strings.TrimSpace(c.Gateway) != "" {
				return c.Gateway, nil
			}
		}
	}
	return "", fmt.Errorf("network %s has no IPv4 gateway in IPAM config", name)
}

func randomSuffix(byteLen int) (string, error) {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random suffix: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func sanitizeShort(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 8 {
		s = s[:8]
	}
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, byte(r))
		case r == '-':
			out = append(out, '-')
		}
	}
	return string(out)
}
