package smoke_test

import (
	"context"
	"net/http"
	"os"
	"testing"
)

var sharedEnv *e2eEnv
var sharedCancel context.CancelFunc

func TestMain(m *testing.M) {
	code := m.Run()
	// Shut down the servers after all tests.
	if sharedCancel != nil {
		sharedCancel()
	}
	if sharedEnv != nil && sharedEnv.cmd != nil {
		_ = sharedEnv.cmd.Wait()
	}
	if ciSharedEnv != nil {
		ciSharedEnv.cancel()
		if ciSharedEnv.cmd != nil {
			_ = ciSharedEnv.cmd.Wait()
		}
		if ciSharedEnv.mockSrv != nil {
			ciSharedEnv.mockSrv.Close()
		}
	}
	os.Exit(code)
}

// setup returns a shared e2eEnv, starting the server once for the whole package.
func setup(t *testing.T) *e2eEnv {
	t.Helper()
	if sharedEnv == nil {
		sharedEnv = newE2EEnv(t)
	}
	return sharedEnv
}

func TestHealth(t *testing.T) {
	env := setup(t)

	resp := env.doRaw("GET", "/health", "", nil)
	m := mustStatus(t, resp, http.StatusOK)
	status := str(t, m, "status")
	if status != "ok" {
		t.Errorf("expected status=ok, got %q", status)
	}
}

func TestReady(t *testing.T) {
	env := setup(t)

	resp := env.doRaw("GET", "/ready", "", nil)
	mustStatus(t, resp, http.StatusOK)
}

func TestVersion(t *testing.T) {
	env := setup(t)

	resp := env.doRaw("GET", "/api/version", "", nil)
	m := mustStatus(t, resp, http.StatusOK)
	version := strOr(m, "current", "")
	if version == "" {
		t.Error("expected non-empty version (key: current)")
	}
	t.Logf("server version: %s", version)
}

func TestFeatures(t *testing.T) {
	env := setup(t)

	resp := env.doRaw("GET", "/api/features", "", nil)
	mustStatus(t, resp, http.StatusOK)
}
