package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/e2e/harness"
	"github.com/clawvisor/clawvisor/internal/e2e/roles"
	"github.com/clawvisor/clawvisor/internal/e2e/scenario"
)

// TestE2E loads every scenario YAML from scenario/library and runs it
// against an in-process harness. Skips when CLAWVISOR_E2E_ANTHROPIC_KEY is
// unset — smoke tests in internal/e2e/harness cover the no-LLM path.
func TestE2E(t *testing.T) {
	if reason, skip := roles.Skip(); skip {
		t.Skip(reason)
	}
	apiKey := os.Getenv(roles.EnvAPIKey)

	libraryDir := filepath.Join("scenario", "library")
	entries, err := os.ReadDir(libraryDir)
	if err != nil {
		t.Fatalf("read library dir: %v", err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		paths = append(paths, filepath.Join(libraryDir, e.Name()))
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Skip("no scenarios in library")
	}

	for _, path := range paths {
		path := path
		sc, err := scenario.Load(path)
		if err != nil {
			t.Errorf("load %s: %v", path, err)
			continue
		}
		t.Run(sc.ID, func(t *testing.T) {
			ctx := context.Background()
			h, err := harness.Start(ctx, t.TempDir(), nil)
			if err != nil {
				t.Fatalf("harness.Start: %v", err)
			}
			t.Cleanup(func() { _ = h.Stop(ctx) })

			opts := RunOptions{APIKey: apiKey}
			if testing.Verbose() {
				// Bypass t.Logf for the trace: it stamps every line
				// with a "file.go:NN:" prefix and collapses leading
				// newlines into that header line, which kills the
				// blank-line separators we want between turns.
				opts.Logf = func(format string, args ...any) {
					fmt.Fprintf(os.Stderr, format+"\n", args...)
				}
				if sc.Mission != "" {
					fmt.Fprintf(os.Stderr, "\nmission» %s\n", strings.TrimSpace(sc.Mission))
				}
			}
			res, err := Run(ctx, h, sc, opts)
			if err != nil {
				t.Fatalf("run scenario: %v", err)
			}
			if res.ResponderError != nil {
				t.Fatalf("responder error: %v", res.ResponderError)
			}
			if len(res.HardFailures) > 0 {
				for _, f := range res.HardFailures {
					t.Errorf("hard expectation failed [%d %s]: %s", f.Index, f.Series, f.Reason)
				}
			}
			for _, f := range res.ApproverFails {
				t.Errorf("approver failure: %s", f)
			}
			for _, j := range res.JudgeResults {
				if !j.Pass {
					t.Errorf("soft expectation failed: %s — %s", j.Expectation, j.Reason)
				}
			}
		})
	}
}
