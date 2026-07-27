package scenarios_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// advisoryNeedle is the stable substring of the pre-flip config_schema notice
// Load() emits (pkg/config/config.go). Matched against captured stderr.
const advisoryNeedle = "config_schema marker is older than current"

// proxyLiteProbe issues an unauthenticated POST to the lite-proxy messages
// endpoint and returns the status code. When proxy-lite is enabled the route
// is mounted and rejects the missing agent token (401); when disabled the
// route is absent (404). This lets a scenario distinguish "endpoint serves"
// from "endpoint absent" without needing a real agent token or upstream.
func proxyLiteProbe(t *testing.T, cv *testapp.Server) int {
	t.Helper()
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("probe /api/v1/messages: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestObservePostureMountsProxyLite — an operator who explicitly opts into the
// Observe posture gets a server that boots and serves the proxy-lite endpoint.
// Observe is the opt-in, not the fresh-install default: the wizard now
// pre-selects skill-gateway-only (internal/setup's
// TestWizardDefaultsToSkillGateway), and the byte-for-byte config shape is
// covered by TestWizardWritesMarkerAndExplicitKey. Here we boot a server on an
// explicit schema-2 Observe config and assert it becomes ready, the proxy-lite
// endpoint is served, and the pre-flip advisory does not fire.
func TestObservePostureMountsProxyLite(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.StartWithConfig(t, h, nil, "config_schema: 2\nposture: observe")

	// /ready is implied by StartWithConfig returning (it polls /ready), but
	// assert explicitly so the contract is visible in this test.
	resp, err := cv.Client.Get(cv.URL + "/ready")
	if err != nil {
		t.Fatalf("/ready: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ready status=%d, want 200", resp.StatusCode)
	}

	if got := proxyLiteProbe(t, cv); got == http.StatusNotFound {
		t.Fatalf("proxy-lite endpoint not served in Observe (got 404); the fresh wizard config must mount it")
	}

	if strings.Contains(cv.Stderr(), advisoryNeedle) {
		t.Fatalf("schema-2 wizard config must NOT trip the pre-flip advisory; stderr:\n%s", cv.Stderr())
	}
}

// TestFlipPreFlipConfigUnchanged — an existing pre-flip config (no proxy_lite
// section, no schema marker: byte-for-byte what old wizards produced) boots
// with proxy-lite OFF and only an advisory log line, proving "no silent
// enablement" by construction (spec 08 / PRD §12). The lite-proxy route must
// be absent (404) and the advisory must fire exactly once.
func TestFlipPreFlipConfigUnchanged(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.StartWithoutProxyLite(t, h, nil)

	if got := proxyLiteProbe(t, cv); got != http.StatusNotFound {
		t.Fatalf("pre-flip config must leave the proxy-lite endpoint absent; probe status=%d, want 404", got)
	}

	stderr := cv.Stderr()
	if n := strings.Count(stderr, advisoryNeedle); n != 1 {
		t.Fatalf("pre-flip advisory should fire exactly once; found %d occurrences.\nstderr:\n%s", n, stderr)
	}
}
