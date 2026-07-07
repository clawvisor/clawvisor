package scenarios_test

import (
	"net/http"
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

// TestVersionEndpoint — /api/version answers 200 with a JSON object
// carrying a "version" field. Dev builds may set the field to empty
// string, so we do NOT assert on the value here — cvGet already
// enforces the 200-status contract and successful JSON decode, which
// is what this test covers.
func TestVersionEndpoint(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)

	var ver struct {
		Version string `json:"version"`
	}
	cvGet(t, cv, "", "/api/version", &ver)
	_ = ver.Version
}
