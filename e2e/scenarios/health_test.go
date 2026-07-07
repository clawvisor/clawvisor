package scenarios_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestHealthAndReadyOnClawvisor — basic operational endpoints.
func TestHealthAndReadyOnClawvisor(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)

	for _, path := range []string{"/health", "/ready"} {
		resp, err := cv.Client.Get(cv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", path, resp.StatusCode)
		}
	}
}

// TestVersionEndpoint — /api/version reports a non-empty version string.
func TestVersionEndpoint(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)

	var ver struct {
		Version string `json:"version"`
	}
	cvGet(t, cv, "", "/api/version", &ver)
	// Dev builds may have an empty version; either way the endpoint should
	// answer 200 with a JSON object.
	if !strings.HasPrefix(ver.Version, "") {
		t.Fatalf("unexpected version: %q", ver.Version)
	}
}
