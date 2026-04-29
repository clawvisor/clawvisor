package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/internal/vault"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestRuntimeProxySwapsScopedPlaceholders(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-placeholder-proxy.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}

	userID, agentID := seedRuntimePrincipal(t, st)
	otherAgent, err := st.CreateAgent(ctx, userID, "other-agent", "other-token-hash")
	if err != nil {
		t.Fatalf("CreateAgent(other): %v", err)
	}
	if err := v.Set(ctx, userID, "mock.placeholder", []byte(`{"token":"real-secret"}`)); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}
	placeholder, err := autovault.GeneratePlaceholder(autovault.PlaceholderPrefix("mock.placeholder"))
	if err != nil {
		t.Fatalf("GeneratePlaceholder: %v", err)
	}
	if err := st.CreateRuntimePlaceholder(ctx, &store.RuntimePlaceholder{
		Placeholder: placeholder,
		UserID:      userID,
		AgentID:     agentID,
		ServiceID:   "mock.placeholder",
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()

	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	ownerSession := createRuntimeSession(t, st, "session-owner", userID, agentID, false)
	otherSession := createRuntimeSession(t, st, "session-other", userID, otherAgent.ID, false)

	srv, err := NewServer(Config{DataDir: cfg.RuntimeProxy.DataDir, Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.InstallSessionGuard(&Authenticator{Store: st})
	srv.InstallPlaceholderSwap(PlaceholderHooks{Store: st, Vault: v})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+ownerSession.secret)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected upstream success, got %d %q", resp.StatusCode, string(body))
	}
	if seenAuth != "Bearer real-secret" {
		t.Fatalf("expected swapped Authorization header, got %q", seenAuth)
	}

	meta, err := st.GetRuntimePlaceholder(ctx, placeholder)
	if err != nil {
		t.Fatalf("GetRuntimePlaceholder: %v", err)
	}
	if meta.LastUsedAt == nil {
		t.Fatal("expected placeholder last_used_at to be updated")
	}

	req, _ = http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+otherSession.secret)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("cross-agent proxy request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected cross-agent placeholder rejection, got %d", resp.StatusCode)
	}
}
