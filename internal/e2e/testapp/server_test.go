package testapp_test

import (
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/internal/e2e/testapp"
	"github.com/clawvisor/clawvisor/internal/testharness"
)

// TestServerBoots is the smoke test for the subprocess boot pattern:
// build the binary, start it on a free port, hit /ready, shut down via
// t.Cleanup. Validates the testapp pipeline end-to-end.
func TestServerBoots(t *testing.T) {
	h := testharness.New(t)
	s := testapp.Start(t, h)
	if s.URL == "" {
		t.Fatal("server URL empty")
	}
	resp, err := s.Client.Get(s.URL + "/ready")
	if err != nil {
		t.Fatalf("GET /ready: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ready status=%d", resp.StatusCode)
	}
}

// TestLoginAsLocalUser exercises the magic-link fixture: server boots,
// LoginAsLocalUser drives /api/auth/magic/local + /api/auth/magic, and
// returns tokens we can use on a subsequent authenticated request.
func TestLoginAsLocalUser(t *testing.T) {
	h := testharness.New(t)
	s := testapp.Start(t, h)
	user := s.LoginAsLocalUser(t)
	if user.AccessToken == "" {
		t.Fatal("LoginAsLocalUser returned empty access token")
	}
	if user.UserID == "" {
		t.Fatal("LoginAsLocalUser returned empty user_id")
	}
}

// TestCreateAgent confirms an agent can be created under a logged-in
// user and the returned token is non-empty.
func TestCreateAgent(t *testing.T) {
	h := testharness.New(t)
	s := testapp.Start(t, h)
	user := s.LoginAsLocalUser(t)
	a := s.CreateAgent(t, user, "smoke-agent")
	if a.ID == "" {
		t.Fatal("agent ID empty")
	}
	if a.Token == "" {
		t.Fatal("agent token empty")
	}
}
