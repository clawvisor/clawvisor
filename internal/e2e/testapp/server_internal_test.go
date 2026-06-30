package testapp

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/testharness"
)

// TestTryStartReturnsEarlyExitErrorOnBindFailure exercises the symptom
// of a port collision: the subprocess exits immediately because bind
// failed. We simulate it with a shell stub that exits 1 — semantically
// identical to clawvisor-server hitting EADDRINUSE and dying.
//
// Without the earlyExitError fix, the readiness loop would spin until
// the 20-second timeout (since /ready never comes up), turning a flaky
// 100ms recovery into a flaky 20-second test failure under load.
func TestTryStartReturnsEarlyExitErrorOnBindFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub uses /bin/sh")
	}
	stub := writeFailingStub(t)
	h := testharness.New(t)

	_, err := tryStart(t, h, nil, stub)
	if err == nil {
		t.Fatal("tryStart returned nil; expected earlyExitError")
	}
	var earlyExit *earlyExitError
	if !errors.As(err, &earlyExit) {
		t.Fatalf("tryStart returned %T (%v); expected earlyExitError so StartWith retries the next port", err, err)
	}
}

// TestWaitReadyReportsEarlyExitFast — the readiness loop must observe
// the dead subprocess within one poll cycle (~150ms), not wait out the
// full 20-second timeout. Validates the channel-based detection rather
// than only checking errors.As shape.
func TestWaitReadyReportsEarlyExitFast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub uses /bin/sh")
	}
	stub := writeFailingStub(t)
	h := testharness.New(t)

	start := time.Now()
	_, err := tryStart(t, h, nil, stub)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error")
	}
	// 2s is generous — failing stub exits in <50ms; poll interval is 150ms.
	// If the channel-based detection regressed to polling-only we'd see ~20s.
	if elapsed > 2*time.Second {
		t.Fatalf("tryStart took %v before reporting early exit; the early-exit channel detection regressed", elapsed)
	}
}

// writeFailingStub writes a shell script that exits non-zero immediately.
// /bin/false would do, but a per-test script keeps the harness portable
// across distros that put it in different paths.
func writeFailingStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "failing-server")
	const script = "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return stub
}
