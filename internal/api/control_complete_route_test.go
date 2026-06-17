package api_test

import (
	"net/http"
	"testing"
)

// TestControlPlaneCompleteRoute_RegisteredAheadOfCatchAll guards
// against two refactor pitfalls:
//
//  1. The route must actually be mounted on the proxy-lite route set
//     so the agent's curl can reach it. A missing route lands on the
//     `/api/control/` catch-all (controlHandler.NotFound) and returns
//     404 — looks identical to "endpoint missing" from the client.
//  2. The catch-all must NOT be mounted before the specific route, or
//     the specific route is silently shadowed.
//
// A 401 (missing caller-auth) means the specific route handler was
// reached and the auth middleware rejected the request — which is the
// correct behavior for an anonymous probe. 404 means the catch-all
// captured the request, which is the regression we want to catch.
func TestControlPlaneCompleteRoute_RegisteredAheadOfCatchAll(t *testing.T) {
	env := newRouteSetEnv(t, "proxy_lite")

	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/api/control/tasks/some-id/complete", nil)
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/control/tasks/.../complete: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("status = 404 — the catch-all /api/control/ swallowed the request, the specific complete route is not registered or is shadowed")
	}
	// Unauthenticated is the expected shape: the caller-nonce middleware
	// rejects the missing X-Clawvisor-Caller header.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (missing caller-auth); 404 would mean the route isn't registered", resp.StatusCode)
	}
}
