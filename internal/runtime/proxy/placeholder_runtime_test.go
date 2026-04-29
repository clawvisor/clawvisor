package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	srv.InstallPlaceholderSwap(PlaceholderHooks{Store: st, Vault: v, Config: cfg})
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

func TestRuntimeProxyObservesKnownOutboundCredentialWithoutCapturing(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-outbound-auto.db"))
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
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()
	cfg.RuntimePolicy.AutovaultMode = "observe"

	rawToken := "ghp_outboundSecret123456789"
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	session := createRuntimeSession(t, st, "session-outbound-auto", userID, agentID, false)
	srv, err := NewServer(Config{DataDir: cfg.RuntimeProxy.DataDir, Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.InstallSessionGuard(&Authenticator{Store: st})
	srv.InstallPlaceholderSwap(PlaceholderHooks{Store: st, Vault: v, Config: cfg})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || seenAuth != "Bearer "+rawToken {
		t.Fatalf("expected raw header to pass through unchanged, status=%d auth=%q", resp.StatusCode, seenAuth)
	}
	if _, ok := lookupRuntimeSecretPlaceholder(srv, &store.RuntimeSession{AgentID: agentID}, rawToken); ok {
		t.Fatal("expected outbound raw credential observation to avoid silent capture")
	}
	events, err := st.ListRuntimeEvents(ctx, userID, store.RuntimeEventFilter{SessionID: session.id, Limit: 10})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	found := false
	for _, event := range events {
		if event.EventType == "runtime.autovault.observed" {
			found = true
			if strings.Contains(string(event.MetadataJSON), rawToken) {
				t.Fatalf("runtime event leaked raw credential: %s", string(event.MetadataJSON))
			}
		}
	}
	if !found {
		t.Fatalf("expected runtime.autovault.observed event, got %+v", events)
	}
}

func TestRuntimeProxyObservesUnknownOutboundCredentialInObserveMode(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-outbound-observe.db"))
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
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()
	cfg.RuntimePolicy.AutovaultMode = "observe"

	rawToken := "ZXhhbXBsZV9iZWFyZXJfdG9rZW5fMTIzNDU2Nzg5"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	session := createRuntimeSession(t, st, "session-outbound-observe", userID, agentID, false)
	srv, err := NewServer(Config{DataDir: cfg.RuntimeProxy.DataDir, Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.InstallSessionGuard(&Authenticator{Store: st})
	srv.InstallPlaceholderSwap(PlaceholderHooks{Store: st, Vault: v, Config: cfg})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	resp.Body.Close()
	if _, ok := lookupRuntimeSecretPlaceholder(srv, &store.RuntimeSession{AgentID: agentID}, rawToken); ok {
		t.Fatal("observe mode should not capture unknown outbound credentials")
	}
	events, err := st.ListRuntimeEvents(ctx, userID, store.RuntimeEventFilter{SessionID: session.id, Limit: 10})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	found := false
	for _, event := range events {
		if event.EventType == "runtime.autovault.observed" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected runtime.autovault.observed event, got %+v", events)
	}
}

func TestInjectStoredBearerUsesKnownHostService(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-inject-bearer.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, _ := seedRuntimePrincipal(t, st)
	if err := v.Set(ctx, userID, "github", []byte(`{"token":"ghp_stored_value"}`)); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	injected, err := injectStoredBearer(req, v, userID)
	if err != nil {
		t.Fatalf("injectStoredBearer: %v", err)
	}
	if !injected {
		t.Fatal("expected injectStoredBearer to report injected=true")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ghp_stored_value" {
		t.Fatalf("expected injected bearer, got %q", got)
	}
}

func TestRuntimeProxyStrictModeRequiresCredentialReview(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-outbound-strict.db"))
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
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()
	cfg.RuntimePolicy.AutovaultMode = "strict"

	rawToken := "ghp_strictSecret123456789"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	session := createRuntimeSession(t, st, "session-outbound-strict", userID, agentID, false)
	srv, err := NewServer(Config{DataDir: cfg.RuntimeProxy.DataDir, Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.InstallSessionGuard(&Authenticator{Store: st})
	srv.InstallPlaceholderSwap(PlaceholderHooks{Store: st, Vault: v, Config: cfg})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected strict mode credential block, got %d body=%s", resp.StatusCode, string(body))
	}
	if strings.Contains(string(body), rawToken) {
		t.Fatalf("response leaked raw credential: %s", string(body))
	}
	records, err := st.ListPendingApprovalRecords(ctx, userID)
	if err != nil {
		t.Fatalf("ListPendingApprovalRecords: %v", err)
	}
	if len(records) != 1 || records[0].Kind != "credential_review" {
		t.Fatalf("expected single credential_review approval, got %+v", records)
	}
	if strings.Contains(string(records[0].SummaryJSON), rawToken) || strings.Contains(string(records[0].PayloadJSON), rawToken) {
		t.Fatalf("approval record leaked raw credential: summary=%s payload=%s", string(records[0].SummaryJSON), string(records[0].PayloadJSON))
	}
}

func TestRuntimeProxyStrictModeHonorsStandingCredentialAuthorization(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-outbound-strict-standing.db"))
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
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()
	cfg.RuntimePolicy.AutovaultMode = "strict"

	rawToken := "ghp_authorizedSecret123456789"
	detection := detectHeaderCredential(mustRequest(t, "https://api.github.com/user"), "Authorization", "Bearer "+rawToken)
	if detection == nil {
		t.Fatal("expected outbound credential detection")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	session := createRuntimeSession(t, st, "session-outbound-strict-standing", userID, agentID, false)
	auth := &store.CredentialAuthorization{
		ID:            "standing-auth",
		UserID:        userID,
		AgentID:       agentID,
		Scope:         "standing",
		CredentialRef: detection.CredentialRef,
		Service:       detection.Service,
		Host:          requestHost(mustRequest(t, upstream.URL)),
		HeaderName:    "Authorization",
		Scheme:        detection.Scheme,
		Status:        "active",
	}
	if err := st.CreateCredentialAuthorization(ctx, auth); err != nil {
		t.Fatalf("CreateCredentialAuthorization: %v", err)
	}

	srv, err := NewServer(Config{DataDir: cfg.RuntimeProxy.DataDir, Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.InstallSessionGuard(&Authenticator{Store: st})
	srv.InstallPlaceholderSwap(PlaceholderHooks{Store: st, Vault: v, Config: cfg})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected standing credential authorization to allow request, got %d", resp.StatusCode)
	}
	events, err := st.ListRuntimeEvents(ctx, userID, store.RuntimeEventFilter{SessionID: session.id, Limit: 10})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	assertEventTypePresent(t, events, "runtime.autovault.observed")
	assertEventTypePresent(t, events, "runtime.autovault.authorized")
}

func mustRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	return req
}

func assertEventTypePresent(t *testing.T, events []*store.RuntimeEvent, want string) {
	t.Helper()
	for _, event := range events {
		if event.EventType == want {
			return
		}
	}
	t.Fatalf("expected runtime event %q, got %+v", want, events)
}
