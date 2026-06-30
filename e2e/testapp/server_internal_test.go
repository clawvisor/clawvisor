package testapp

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/testharness"
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

// TestEarlyExitErrorCarriesSubprocessDiagnostics — the symptom this
// fix targets: when retries discard the failed *Server, the buffered
// stderr (where the real "bind: address already in use" would be)
// must NOT be lost. The earlyExitError returned by tryStart must
// carry those bytes so the final t.Fatalf surfaces them.
func TestEarlyExitErrorCarriesSubprocessDiagnostics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub uses /bin/sh")
	}
	// Stub emits a distinct sentinel on both streams, then exits 1.
	// Matching the real bind-failure shape: stderr message + nonzero exit.
	dir := t.TempDir()
	stub := filepath.Join(dir, "failing-server")
	const script = "#!/bin/sh\n" +
		"echo 'stdout-sentinel-line'\n" +
		"echo 'bind: address already in use' 1>&2\n" +
		"exit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	h := testharness.New(t)

	_, err := tryStart(t, h, nil, stub)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bind: address already in use") {
		t.Fatalf("error did not include stderr diagnostics; would lose root-cause info on retry exhaustion.\nGot: %q", msg)
	}
	if !strings.Contains(msg, "stdout-sentinel-line") {
		t.Fatalf("error did not include stdout diagnostics.\nGot: %q", msg)
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
