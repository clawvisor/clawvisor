package isolation

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// emptyNetworkGracePeriod gates removal of label-matched networks that have
// no attached containers — bootstrapping invocations briefly look idle.
const emptyNetworkGracePeriod = 5 * time.Minute

// PruneStale removes orphaned isolation networks and containers.
// Best-effort: never returns an error to the caller; cheap to call inline at
// the start of every isolated invocation.
func PruneStale(ctx context.Context, dockerBin string) {
	pruneOrphanContainers(ctx, dockerBin)
	pruneIdleNetworks(ctx, dockerBin)
}

func pruneOrphanContainers(ctx context.Context, dockerBin string) {
	exited, _ := listContainerIDs(ctx, dockerBin, "exited")
	for _, id := range exited {
		_ = removeContainer(ctx, dockerBin, id)
	}
	running, _ := listContainerIDs(ctx, dockerBin, "running")
	for _, id := range running {
		pid := containerOwnerPID(ctx, dockerBin, id)
		if pid <= 0 {
			continue
		}
		if processAlive(pid) {
			continue
		}
		_ = StopContainer(ctx, dockerBin, id)
		_ = removeContainer(ctx, dockerBin, id)
	}
}

func pruneIdleNetworks(ctx context.Context, dockerBin string) {
	out, err := exec.CommandContext(ctx, dockerBin,
		"network", "ls", "--filter", "label="+LabelIsolation, "--format", "{{.Name}}",
	).Output()
	if err != nil {
		return
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !networkIsIdle(ctx, dockerBin, name) {
			continue
		}
		_ = removeNetwork(ctx, dockerBin, name)
	}
}

type pruneNetworkInspect struct {
	Containers map[string]any   `json:"Containers"`
	Labels     map[string]string `json:"Labels"`
}

func networkIsIdle(ctx context.Context, dockerBin, name string) bool {
	out, err := exec.CommandContext(ctx, dockerBin, "network", "inspect", name).Output()
	if err != nil {
		return false
	}
	var nets []pruneNetworkInspect
	if err := json.Unmarshal(out, &nets); err != nil {
		return false
	}
	if len(nets) == 0 {
		return false
	}
	n := nets[0]
	if len(n.Containers) > 0 {
		return false
	}
	createdRaw := strings.TrimSpace(n.Labels[LabelKeyCreated])
	if createdRaw == "" {
		return true
	}
	created, err := time.Parse(time.RFC3339, createdRaw)
	if err != nil {
		return true
	}
	return time.Since(created) > emptyNetworkGracePeriod
}

func listContainerIDs(ctx context.Context, dockerBin, status string) ([]string, error) {
	args := []string{
		"ps",
		"--filter", "label=" + LabelIsolation,
		"--format", "{{.ID}}",
	}
	if status != "" {
		args = append(args, "--filter", "status="+status)
		if status == "exited" {
			args = append(args, "-a")
		}
	}
	out, err := exec.CommandContext(ctx, dockerBin, args...).Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	ids := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			ids = append(ids, l)
		}
	}
	return ids, nil
}

type containerInspect struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func containerOwnerPID(ctx context.Context, dockerBin, id string) int {
	out, err := exec.CommandContext(ctx, dockerBin, "inspect", id).Output()
	if err != nil {
		return 0
	}
	var arr []containerInspect
	if err := json.Unmarshal(out, &arr); err != nil || len(arr) == 0 {
		return 0
	}
	raw := strings.TrimSpace(arr[0].Config.Labels[LabelKeyOwnerPID])
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.ESRCH) {
		return false
	}
	// EPERM means the process exists but we can't signal it — it's alive.
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}

func removeContainer(ctx context.Context, dockerBin, id string) error {
	cmd := exec.CommandContext(ctx, dockerBin, "rm", "-f", id)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

