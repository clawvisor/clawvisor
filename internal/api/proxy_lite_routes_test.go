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
	"github.com/clawvisor/clawvisor/pkg/store"
	sqlitestore "github.com/clawvisor/clawvisor/pkg/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/vault"
	intvault "github.com/clawvisor/clawvisor/pkg/vault"
)

func TestProxyLiteRouteSetExposesOnlyProxySurface(t *testing.T) {
	env := newRouteSetEnv(t, "proxy_lite")

	assertStatus(t, env.ts.Client(), env.ts.URL+"/health", http.StatusOK)
	assertStatus(t, env.ts.Client(), env.ts.URL+"/control", http.StatusOK)
	assertStatus(t, env.ts.Client(), env.ts.URL+"/api/features", http.StatusNotFound)
	assertStatus(t, env.ts.Client(), env.ts.URL+"/api/runtime/llm-credentials", http.StatusNotFound)
}

func TestAppRouteSetHidesProxySurfaceButKeepsManagementRoutes(t *testing.T) {
	env := newRouteSetEnv(t, "app")

	assertStatus(t, env.ts.Client(), env.ts.URL+"/health", http.StatusOK)
	assertStatus(t, env.ts.Client(), env.ts.URL+"/control", http.StatusNotFound)
	assertStatus(t, env.ts.Client(), env.ts.URL+"/v1/messages", http.StatusNotFound)

	// Route exists and rejects missing user auth. This is what lets the main
	// app own credential management while a separate service owns proxy traffic.
	assertStatus(t, env.ts.Client(), env.ts.URL+"/api/runtime/llm-credentials", http.StatusUnauthorized)
}

type routeSetEnv struct {
	ts    *httptest.Server
	Store store.Store
	Vault vault.Vault
}

func newRouteSetEnv(t *testing.T, routeSet string) *routeSetEnv {
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
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0, RouteSet: routeSet},
		Auth: config.AuthConfig{
			JWTSecret:       "test-secret-for-integration-tests",
			AccessTokenTTL:  "15m",
			RefreshTokenTTL: "720h",
		},
		Approval:  config.ApprovalConfig{Timeout: 300, OnTimeout: "fail"},
		Task:      config.TaskConfig{DefaultExpirySeconds: 3600},
		ProxyLite: config.ProxyLiteConfig{Enabled: true},
	}

	srv, err := api.New(cfg, st, v, jwtSvc, adapters.NewRegistry(), nil, config.LLMConfig{}, nil)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = st.Close() })
	return &routeSetEnv{ts: ts, Store: st, Vault: v}
}

func assertStatus(t *testing.T, c *http.Client, url string, want int) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("GET %s status=%d, want %d", url, resp.StatusCode, want)
	}
}
