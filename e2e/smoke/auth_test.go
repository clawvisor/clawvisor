package smoke_test

import (
	"net/http"
	"testing"
)

func TestMe(t *testing.T) {
	env := setup(t)

	resp := env.userDo("GET", "/api/me", nil)
	m := mustStatus(t, resp, http.StatusOK)

	// /api/me returns the user directly, not wrapped in {"user": ...}.
	email := str(t, m, "email")
	if email != "admin@local" {
		t.Errorf("expected admin@local, got %q", email)
	}
}

func TestTokenRefresh(t *testing.T) {
	env := setup(t)

	resp := env.doRaw("POST", "/api/auth/refresh", "", map[string]any{
		"refresh_token": env.UserRefreshToken,
	})
	m := mustStatus(t, resp, http.StatusOK)
	newAccess := str(t, m, "access_token")
	if newAccess == "" {
		t.Error("expected non-empty access_token after refresh")
	}
}

func TestUnauthenticatedReturns401(t *testing.T) {
	env := setup(t)

	resp := env.doRaw("GET", "/api/me", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}
