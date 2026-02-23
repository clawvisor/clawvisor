package api_test

import (
	"fmt"
	"net/http"
	"testing"
)

func TestAuth_Register(t *testing.T) {
	env := newTestEnv(t)
	email := fmt.Sprintf("alice-%s@test.example", randSuffix())

	resp := env.do("POST", "/api/auth/register", "", map[string]any{
		"email": email, "password": "secret123",
	})
	body := mustStatus(t, resp, http.StatusCreated)

	// Response must contain user object and tokens
	user := nested(t, body, "user")
	if str(t, user, "email") != email {
		t.Errorf("register: user.email mismatch")
	}
	if str(t, user, "id") == "" {
		t.Error("register: user.id is empty")
	}
	if str(t, body, "access_token") == "" {
		t.Error("register: access_token missing")
	}
	if str(t, body, "refresh_token") == "" {
		t.Error("register: refresh_token missing")
	}
}

func TestAuth_Register_DuplicateEmail(t *testing.T) {
	env := newTestEnv(t)
	email := fmt.Sprintf("bob-%s@test.example", randSuffix())

	env.do("POST", "/api/auth/register", "", map[string]any{
		"email": email, "password": "password-one",
	}) // ignore first response

	resp := env.do("POST", "/api/auth/register", "", map[string]any{
		"email": email, "password": "password-two",
	})
	mustStatus(t, resp, http.StatusConflict)
}

func TestAuth_Register_MissingFields(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("POST", "/api/auth/register", "", map[string]any{"email": "x@x.com"})
	mustStatus(t, resp, http.StatusBadRequest)

	resp = env.do("POST", "/api/auth/register", "", map[string]any{"password": "pass"})
	mustStatus(t, resp, http.StatusBadRequest)
}

func TestAuth_Login(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := env.do("POST", "/api/auth/login", "", map[string]any{
		"email": s.Email, "password": "TestPass123!",
	})
	body := mustStatus(t, resp, http.StatusOK)

	if str(t, body, "access_token") == "" {
		t.Error("login: access_token missing")
	}
	user := nested(t, body, "user")
	if str(t, user, "email") != s.Email {
		t.Errorf("login: user.email mismatch")
	}
}

func TestAuth_Login_WrongPassword(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := env.do("POST", "/api/auth/login", "", map[string]any{
		"email": s.Email, "password": "wrong",
	})
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestAuth_Login_UnknownEmail(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("POST", "/api/auth/login", "", map[string]any{
		"email": "nobody@test.example", "password": "pass",
	})
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestAuth_Me_RequiresToken(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("GET", "/api/me", "", nil) // no token
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestAuth_Me_ReturnsUser(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("GET", "/api/me", nil)
	body := mustStatus(t, resp, http.StatusOK)

	if str(t, body, "email") != s.Email {
		t.Errorf("me: email mismatch, got %q", str(t, body, "email"))
	}
	if str(t, body, "id") != s.UserID {
		t.Errorf("me: id mismatch")
	}
}

func TestAuth_Refresh(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := env.do("POST", "/api/auth/refresh", "", map[string]any{
		"refresh_token": s.RefreshToken,
	})
	body := mustStatus(t, resp, http.StatusOK)

	newToken := str(t, body, "access_token")
	if newToken == "" {
		t.Error("refresh: access_token missing")
	}
	// New token should also work
	resp2 := env.do("GET", "/api/me", newToken, nil)
	mustStatus(t, resp2, http.StatusOK)
}

func TestAuth_Logout_InvalidatesRefreshToken(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Confirm both tokens work before logout
	resp := s.do("GET", "/api/me", nil)
	mustStatus(t, resp, http.StatusOK)

	// Logout with refresh token in body (deletes the session)
	resp = env.do("POST", "/api/auth/logout", s.AccessToken, map[string]any{
		"refresh_token": s.RefreshToken,
	})
	mustStatus(t, resp, http.StatusNoContent)

	// Refresh token must now be rejected (session deleted server-side).
	// Note: the short-lived access JWT remains valid until its TTL — this is
	// expected behaviour for stateless JWTs without a token blacklist.
	resp = env.do("POST", "/api/auth/refresh", "", map[string]any{
		"refresh_token": s.RefreshToken,
	})
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestAuth_InvalidToken_Rejected(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("GET", "/api/me", "not-a-valid-jwt", nil)
	mustStatus(t, resp, http.StatusUnauthorized)
}
