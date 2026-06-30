package scenarios_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestLLMProxyControlCapabilities — /api/control/capabilities is public
// (no auth) and returns the proxy's exposed control surface as JSON.
// Agents fetch this to discover what they can do.
func TestLLMProxyControlCapabilities(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	for _, path := range []string{"/api/control", "/api/control/capabilities"} {
		resp, err := cv.Client.Get(cv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body := readBodyStr(resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, resp.StatusCode, body)
		}
		if !strings.HasPrefix(body, "{") && !strings.HasPrefix(body, "[") {
			t.Fatalf("GET %s returned non-JSON: %s", path, body)
		}
	}
}

// TestLLMProxyControlSkill — /api/control/skill returns the Clawvisor
// skill bundle (markdown agent instructions).
func TestLLMProxyControlSkill(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	resp, err := cv.Client.Get(cv.URL + "/api/control/skill")
	if err != nil {
		t.Fatalf("GET /api/control/skill: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// TestLLMProxyControlAutovaultScriptDocs — /api/control/autovault/script
// returns docs for the autovault script language. Public endpoint agents
// hit before minting a script session.
func TestLLMProxyControlAutovaultScriptDocs(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	resp, err := cv.Client.Get(cv.URL + "/api/control/autovault/script")
	if err != nil {
		t.Fatalf("GET autovault script: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// TestLLMProxyControlListTasksRequiresAgent — /api/control/tasks needs
// agent auth via the nonce middleware.
func TestLLMProxyControlListTasksRequiresAgent(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	resp, err := cv.Client.Get(cv.URL + "/api/control/tasks")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("unauth /api/control/tasks succeeded (status=%d)", resp.StatusCode)
	}
}
