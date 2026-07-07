package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
	sqlitestore "github.com/clawvisor/clawvisor/pkg/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/pkg/vault"
)

// newAPITokensEnv builds an API server whose FeatureSet.APITokens matches the
// given flag, so the token mint/manage routes are gated exactly as production
// (registered when enabled, absent → 404 when disabled). Password auth is on
// so the first registered user becomes admin and can drive the routes.
func newAPITokensEnv(t *testing.T, apiTokensEnabled bool) *testEnv {
	t.Helper()
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	st := sqlitestore.NewStore(db)

	v, err := intvault.NewLocalVault(t.TempDir()+"/vault.key", db, "sqlite")
	if err != nil {
		t.Fatalf("vault: %v", err)
	}

	jwtSvc, err := auth.NewJWTService("test-secret-for-integration-tests")
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0},
		Auth: config.AuthConfig{
			JWTSecret:       "test-secret-for-integration-tests",
			AccessTokenTTL:  "15m",
			RefreshTokenTTL: "720h",
		},
		Approval: config.ApprovalConfig{Timeout: 300, OnTimeout: "fail", AllowSelfApprove: true, AdminNotify: true},
		Task:     config.TaskConfig{DefaultExpirySeconds: 3600},
	}

	srv, err := api.New(cfg, st, v, jwtSvc, adapters.NewRegistry(), nil, config.LLMConfig{}, nil,
		api.WithFeatures(api.FeatureSet{PasswordAuth: true, APITokens: apiTokensEnabled}),
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = st.Close() })

	return &testEnv{t: t, ts: ts, Vault: v, Store: st, client: ts.Client()}
}

// TestFeatures_APITokensReflectsFlag pins handleFeatures: /api/features
// advertises api_tokens matching the deployment gate (true when enabled, false
// when disabled) so the Terraform provider capability-negotiates instead of
// hitting a 404/401 on the token routes.
func TestFeatures_APITokensReflectsFlag(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		env := newAPITokensEnv(t, enabled)
		resp := env.do("GET", "/api/features", "", nil)
		body := mustStatus(t, resp, http.StatusOK)
		got, ok := body["api_tokens"].(bool)
		if !ok {
			t.Fatalf("enabled=%v: api_tokens missing or not a bool in %v", enabled, body)
		}
		if got != enabled {
			t.Fatalf("enabled=%v: /api/features api_tokens=%v, want %v", enabled, got, enabled)
		}
	}
}

// TestAPITokensDisabled_RoutesReturn404 pins the route guard: with API tokens
// disabled instance-wide the mint/list/revoke routes are never registered, so
// the Go mux returns 404 for every method — even for a legitimate admin JWT.
func TestAPITokensDisabled_RoutesReturn404(t *testing.T) {
	env := newAPITokensEnv(t, false)
	admin := newSession(t, env) // first registered user is admin

	// The Go mux returns a plain-text (non-JSON) 404 when a route is absent,
	// so assert on the status code directly rather than decoding a body.
	wantStatus := func(method, path string, code int) {
		t.Helper()
		resp := env.do(method, path, admin.AccessToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != code {
			t.Fatalf("%s %s: status=%d want %d", method, path, resp.StatusCode, code)
		}
	}

	wantStatus("POST", "/api/tokens", http.StatusNotFound)
	wantStatus("GET", "/api/tokens", http.StatusNotFound)
	wantStatus("DELETE", "/api/tokens/some-id", http.StatusNotFound)
}

// TestAPITokensEnabled_RoutesWork is the default-enabled counterpart: the
// mint/list/revoke routes are registered and serve an admin JWT (201/200/204).
func TestAPITokensEnabled_RoutesWork(t *testing.T) {
	env := newAPITokensEnv(t, true)
	admin := newSession(t, env) // first registered user is admin

	// POST /api/tokens → 201 Created
	resp := env.do("POST", "/api/tokens", admin.AccessToken, map[string]any{
		"name": "tf", "scope": "instance-admin",
	})
	created := mustStatus(t, resp, http.StatusCreated)
	tokenID := str(t, created, "id")

	// GET /api/tokens → 200 OK
	resp = env.do("GET", "/api/tokens", admin.AccessToken, nil)
	mustStatus(t, resp, http.StatusOK)

	// DELETE /api/tokens/{id} → 204 No Content
	resp = env.do("DELETE", "/api/tokens/"+tokenID, admin.AccessToken, nil)
	mustStatus(t, resp, http.StatusNoContent)
}
