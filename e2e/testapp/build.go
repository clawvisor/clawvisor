// Package testapp boots the clawvisor-server binary as a subprocess
// wired to a fresh harness (internal/testharness). Tests talk to the
// running server via real HTTP — same surface a production client hits.
//
// Subprocess (vs. in-process via internal/e2e/harness) is chosen so the
// full bootstrap, route mounting, and middleware chain run for every
// test — closer to production behavior, at the cost of ~1s per test
// for the spawn + readiness wait.
package testapp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	buildOnce sync.Map // map[string]*sync.Once keyed by pkg path
	buildErr  sync.Map // map[string]error
	buildBin  sync.Map // map[string]string
)

// buildServerBinary compiles cmd/clawvisor-server once per test process
// and returns the path. Subsequent calls within the same process reuse
// the cached binary.
func buildServerBinary(t *testing.T) string {
	t.Helper()
	return buildBinary(t, "github.com/clawvisor/clawvisor/cmd/clawvisor-server")
}

// buildBinary compiles the given Go main package into a process-local
// cache dir keyed by the package path. Within a single `go test`
// invocation all callers share one build; across invocations the cache
// is rebuilt (so stale binaries can never pin behavior).
func buildBinary(t *testing.T, pkg string) string {
	t.Helper()
	once, _ := buildOnce.LoadOrStore(pkg, &sync.Once{})
	once.(*sync.Once).Do(func() {
		dir := cacheDir(pkg)
		if err := os.MkdirAll(dir, 0755); err != nil {
			buildErr.Store(pkg, err)
			return
		}
		bin := filepath.Join(dir, "bin")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, pkg)
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr.Store(pkg, &buildError{pkg: pkg, err: err, out: string(out)})
			return
		}
		buildBin.Store(pkg, bin)
	})
	if err, ok := buildErr.Load(pkg); ok && err != nil {
		t.Fatalf("build %s: %v", pkg, err)
	}
	bin, _ := buildBin.Load(pkg)
	return bin.(string)
}

type buildError struct {
	pkg string
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.out }

func cacheDir(pkg string) string {
	sum := sha256.Sum256([]byte(pkg))
	return filepath.Join(os.TempDir(), "clawvisor-testapp-build", hex.EncodeToString(sum[:8]))
}
