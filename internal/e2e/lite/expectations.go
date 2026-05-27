package lite

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// StepEval evaluates one step's per-step expectations against the
// workspace and returns the failure messages, if any.
func StepEval(ctx context.Context, workspace string, exp StepExpect) []string {
	var fails []string
	for _, p := range exp.FilesAbsent {
		abs := filepath.Join(workspace, p)
		if _, err := os.Stat(abs); err == nil {
			fails = append(fails, fmt.Sprintf("expected absent: %s (exists)", p))
		}
	}
	for _, p := range exp.FilesPresent {
		abs := filepath.Join(workspace, p)
		if _, err := os.Stat(abs); err != nil {
			fails = append(fails, fmt.Sprintf("expected present: %s (missing)", p))
		}
	}
	for _, fc := range exp.FileContains {
		abs := filepath.Join(workspace, fc.Path)
		data, err := os.ReadFile(abs)
		if err != nil {
			fails = append(fails, fmt.Sprintf("file_contains: read %s: %s", fc.Path, err))
			continue
		}
		if !strings.Contains(string(data), fc.Needle) {
			fails = append(fails, fmt.Sprintf("file_contains: %s does not contain %q", fc.Path, fc.Needle))
		}
	}
	for _, sh := range exp.Shell {
		if msg := runShellAssert(ctx, workspace, sh); msg != "" {
			fails = append(fails, msg)
		}
	}
	return fails
}

func runShellAssert(ctx context.Context, workspace string, sh ShellExpect) string {
	cmd := strings.ReplaceAll(sh.Cmd, "${WORKSPACE}", workspace)
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, "sh", "-c", cmd)
	c.Dir = workspace
	out, err := c.CombinedOutput()
	got := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			got = ee.ExitCode()
		} else {
			return fmt.Sprintf("shell %q: run error: %s\n%s", cmd, err, out)
		}
	}
	if got != sh.Exit {
		return fmt.Sprintf("shell %q: exit %d, want %d\n%s", cmd, got, sh.Exit, out)
	}
	return ""
}

// ScenarioEval evaluates scenario-level hard expectations against the
// counter snapshot. Returns failure messages.
func ScenarioEval(hard []HardExpect, snap map[string]int) []string {
	var fails []string
	for _, h := range hard {
		if h.Count == nil {
			continue
		}
		got := snap[h.Count.Series]
		if h.Count.EQ != nil && got != *h.Count.EQ {
			fails = append(fails, fmt.Sprintf("series %s = %d, want eq %d", h.Count.Series, got, *h.Count.EQ))
		}
		if h.Count.GTE != nil && got < *h.Count.GTE {
			fails = append(fails, fmt.Sprintf("series %s = %d, want gte %d", h.Count.Series, got, *h.Count.GTE))
		}
		if h.Count.LTE != nil && got > *h.Count.LTE {
			fails = append(fails, fmt.Sprintf("series %s = %d, want lte %d", h.Count.Series, got, *h.Count.LTE))
		}
	}
	return fails
}
