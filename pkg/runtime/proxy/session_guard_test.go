package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/elazarl/goproxy"

	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestSessionGuardStripsInternalBypassHeader(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "session-guard.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "session-123", userID, agentID, false)

	var seenBypass string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBypass = r.Header.Get(internalBypassHeader)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.InstallSessionGuard(&Authenticator{Store: st})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	req.Header.Set(internalBypassHeader, "1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected upstream success, got %d %q", resp.StatusCode, string(body))
	}
	if seenBypass != "" {
		t.Fatalf("expected internal bypass header to be stripped, got %q", seenBypass)
	}
}

// TestSessionGuardPropagatesAgentContext verifies that authenticated requests
// carry a synthesized *store.Agent in the request context (UserID, AgentID,
// OrgID from the runtime session), so downstream hooks like
// OrgAwareVault.Resolve can reach org-shared credentials without an extra DB
// lookup. Phase 0.4 deliverable.
//
// We capture the context from a goproxy OnRequest hook installed AFTER the
// session guard, since that's the surface where downstream hook code reads
// it. (The upstream HTTP test server can't observe Go context — only headers
// — so its request scope is unsuitable for this assertion.)
func TestSessionGuardPropagatesAgentContext(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "agent-ctx.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	userID, agentID := seedRuntimePrincipal(t, st)

	const wantOrgID = "org-alpha"
	secret := "ctx-secret"
	sess := &store.RuntimeSession{
		ID:                    "ctx-session",
		UserID:                userID,
		AgentID:               agentID,
		OrgID:                 wantOrgID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: HashProxyBearerSecret(secret),
		ExpiresAt:             time.Now().UTC().Add(30 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, sess); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.InstallSessionGuard(&Authenticator{Store: st})

	var seenAgent *store.Agent
	// Install a downstream hook that captures context.
	srv.GoProxy().OnRequest().DoFunc(func(req *http.Request, _ *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		seenAgent = store.AgentFromContext(req.Context())
		return req, nil
	})

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected upstream success, got %d %q", resp.StatusCode, string(body))
	}
	if seenAgent == nil {
		t.Fatal("expected downstream hook to observe an Agent in context, got nil")
	}
	if seenAgent.UserID != userID {
		t.Errorf("agent UserID = %q, want %q", seenAgent.UserID, userID)
	}
	if seenAgent.ID != agentID {
		t.Errorf("agent ID = %q, want %q", seenAgent.ID, agentID)
	}
	if seenAgent.OrgID != wantOrgID {
		t.Errorf("agent OrgID = %q, want %q", seenAgent.OrgID, wantOrgID)
	}
}
