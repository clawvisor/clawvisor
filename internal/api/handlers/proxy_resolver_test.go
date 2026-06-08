package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	"golang.org/x/oauth2"
)

func newSeededResolver(t *testing.T) (*ProxyResolverHandler, store.Store, *store.User, *store.Agent, llmproxy.CallerNonceCache, string) {
	t.Helper()
	ctx := context.Background()

	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "resolver.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "resolver@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rawAgentToken, err := auth.GenerateAgentToken()
	if err != nil {
		t.Fatalf("GenerateAgentToken: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", auth.HashToken(rawAgentToken))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	v := &stubVault{}
	_ = v.Set(ctx, user.ID, "github", []byte("real-gh-token"))

	placeholder, err := autovault.GeneratePlaceholder(autovault.PlaceholderPrefix("github"))
	if err != nil {
		t.Fatalf("GeneratePlaceholder: %v", err)
	}
	if err := st.CreateRuntimePlaceholder(ctx, &store.RuntimePlaceholder{
		Placeholder: placeholder,
		UserID:      user.ID,
		AgentID:     agent.ID,
		ServiceID:   "github",
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}

	h := NewProxyResolverHandler(st, v, slog.Default())
	h.AllowPrivateNetworks = true // allow httptest's loopback target
	nonces := llmproxy.NewMemoryCallerNonceCache(5 * time.Minute)
	return h, st, user, agent, nonces, placeholder
}

// mintTestNonce produces a nonce bound to the given target so a test
// can populate X-Clawvisor-Caller for the resolver middleware. Matches
// what postprocess does for real credentialed rewrites.
func mintTestNonce(t *testing.T, nonces llmproxy.CallerNonceCache, agentID, host, method, path string) string {
	t.Helper()
	nonce, err := nonces.Mint(context.Background(), agentID, llmproxy.NonceTarget{
		Host:   host,
		Method: method,
		Path:   path,
	})
	if err != nil {
		t.Fatalf("Mint nonce: %v", err)
	}
	return nonce
}

// nonceForRequest mints a caller nonce that satisfies the middleware
// for the given outbound request — reads target from the request's
// X-Clawvisor-Target-Host header. CALL ORDER MATTERS: set the target
// header BEFORE invoking this helper; otherwise the nonce binds to an
// empty host and the middleware will reject the request.
func nonceForRequest(t *testing.T, nonces llmproxy.CallerNonceCache, agentID string, req *http.Request) string {
	t.Helper()
	return mintTestNonce(t, nonces, agentID,
		req.Header.Get("X-Clawvisor-Target-Host"),
		req.Method,
		strings.TrimPrefix(req.URL.Path, "/api/proxy"),
	)
}

type resolverOAuthTestAdapter struct {
	serviceID string
	tokenURL  string
}

func (a resolverOAuthTestAdapter) ServiceID() string { return a.serviceID }
func (a resolverOAuthTestAdapter) SupportedActions() []string {
	return []string{"list_messages"}
}
func (a resolverOAuthTestAdapter) Execute(context.Context, adapters.Request) (*adapters.Result, error) {
	return nil, nil
}
func (a resolverOAuthTestAdapter) OAuthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		Endpoint: oauth2.Endpoint{
			TokenURL: a.tokenURL,
		},
	}
}
func (a resolverOAuthTestAdapter) CredentialFromToken(*oauth2.Token) ([]byte, error) { return nil, nil }
func (a resolverOAuthTestAdapter) ValidateCredential([]byte) error                   { return nil }
func (a resolverOAuthTestAdapter) RequiredScopes() []string                          { return nil }

func TestResolver_HappyPath(t *testing.T) {
	var seenHost, seenPath, seenAuth string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	var rawLog bytes.Buffer
	h.RawIOLogger = llmproxy.NewRawIOLogger(&rawLog)

	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	// Target host: api.github.com (in the github bound-service allowlist).
	// The redirectTargetTransport sends the actual dial to httptest's
	// loopback URL, but the resolver believes (and validates against)
	// api.github.com.
	req := httptest.NewRequest(http.MethodGet, "/api/proxy/repos/x/y/issues", strings.NewReader(`{"filter":"open"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "req-raw-resolver")
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	// Harness sends the placeholder in the natural Authorization header.
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if seenHost == "" {
		t.Fatalf("expected upstream Host header to be set")
	}
	if seenPath != "/repos/x/y/issues" {
		t.Fatalf("expected upstream path /repos/x/y/issues, got %q", seenPath)
	}
	if seenAuth != "Bearer real-gh-token" {
		t.Fatalf("expected upstream Authorization=Bearer real-gh-token, got %q", seenAuth)
	}
	if string(seenBody) != `{"filter":"open"}` {
		t.Fatalf("expected upstream body forwarded, got %q", string(seenBody))
	}
	rawLines := strings.Split(strings.TrimSpace(rawLog.String()), "\n")
	if len(rawLines) != 2 {
		t.Fatalf("expected 2 raw resolver log lines, got %d: %s", len(rawLines), rawLog.String())
	}
	var sawRequest, sawResponse bool
	for _, line := range rawLines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("raw log line is not JSON: %v\n%s", err, line)
		}
		switch ev["phase"] {
		case "resolver_received_request":
			sawRequest = true
			if ev["body"] != `{"filter":"open"}` {
				t.Fatalf("resolver request raw body = %v", ev["body"])
			}
			headers, _ := ev["headers"].(map[string]any)
			if _, ok := headers["Authorization"]; ok {
				t.Fatalf("resolver raw request logged Authorization header: %#v", headers)
			}
		case "resolver_harness_response":
			sawResponse = true
			if ev["body"] != `{"ok":true}` {
				t.Fatalf("resolver response raw body = %v", ev["body"])
			}
		}
	}
	if !sawRequest || !sawResponse {
		t.Fatalf("missing resolver raw phases: request=%v response=%v log=%s", sawRequest, sawResponse, rawLog.String())
	}
	if strings.Contains(rawLog.String(), "real-gh-token") {
		t.Fatalf("raw resolver log should not contain swapped credential: %s", rawLog.String())
	}
}

func TestResolver_RefreshesExpiredOAuthCredentialBeforeForwarding(t *testing.T) {
	var refreshRequests int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshRequests++
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-token" {
			t.Fatalf("unexpected refresh request form: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-access-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h, st, user, agent, nonces, _ := newSeededResolver(t)
	reg := adapters.NewRegistry()
	reg.Register(resolverOAuthTestAdapter{serviceID: "google.gmail", tokenURL: tokenSrv.URL})
	h.AdapterReg = reg
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	credential, err := json.Marshal(map[string]any{
		"type":          "oauth2",
		"access_token":  "expired-access-token",
		"refresh_token": "refresh-token",
		"expiry":        time.Now().UTC().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("Marshal credential: %v", err)
	}
	const vaultKey = "google:eric@example.com"
	if err := h.Vault.Set(context.Background(), user.ID, vaultKey, credential); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	const grantID = "grant-google"
	if err := st.CreateCredentialAuthorization(context.Background(), &store.CredentialAuthorization{
		ID:            grantID,
		UserID:        user.ID,
		AgentID:       agent.ID,
		Scope:         "session",
		CredentialRef: vaultKey,
		Service:       "google.gmail:eric@example.com",
		Host:          "gmail.googleapis.com",
		HeaderName:    "authorization",
		Scheme:        "bearer",
		Status:        "active",
		ExpiresAt:     &expiresAt,
	}); err != nil {
		t.Fatalf("CreateCredentialAuthorization: %v", err)
	}
	placeholder, err := autovault.GeneratePlaceholder(autovault.PlaceholderPrefix("google.gmail:eric@example.com"))
	if err != nil {
		t.Fatalf("GeneratePlaceholder: %v", err)
	}
	if err := st.CreateRuntimePlaceholder(context.Background(), &store.RuntimePlaceholder{
		Placeholder:       placeholder,
		UserID:            user.ID,
		AgentID:           agent.ID,
		ServiceID:         "google.gmail:eric@example.com",
		VaultItemID:       "google.gmail:eric@example.com",
		CredentialGrantID: grantID,
		ExpiresAt:         &expiresAt,
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/gmail/v1/users/me/messages", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "gmail.googleapis.com")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if refreshRequests != 1 {
		t.Fatalf("expected one refresh request, got %d", refreshRequests)
	}
	if seenAuth != "Bearer fresh-access-token" {
		t.Fatalf("expected refreshed access token upstream, got %q", seenAuth)
	}
}

// Malformed `expiry` in a stored OAuth credential must fail closed with
// OAUTH_CREDENTIAL_INVALID rather than silently falling back to the
// legacy ExtractCredentialValue path, which would lift the stale
// access_token verbatim and ship it upstream (disabling refresh).
func TestResolver_MalformedOAuthExpiryFailsClosed(t *testing.T) {
	var refreshRequests int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshRequests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-access-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h, st, user, agent, nonces, _ := newSeededResolver(t)
	reg := adapters.NewRegistry()
	reg.Register(resolverOAuthTestAdapter{serviceID: "google.gmail", tokenURL: tokenSrv.URL})
	h.AdapterReg = reg
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	// Hand-rolled JSON so `expiry` is an unparseable string — encoding/json
	// would otherwise emit a valid RFC3339 timestamp for a time.Time field.
	credential := []byte(`{"type":"oauth2","access_token":"expired-access-token","refresh_token":"refresh-token","expiry":"not-a-date"}`)
	const vaultKey = "google:eric@example.com"
	if err := h.Vault.Set(context.Background(), user.ID, vaultKey, credential); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	const grantID = "grant-google"
	if err := st.CreateCredentialAuthorization(context.Background(), &store.CredentialAuthorization{
		ID:            grantID,
		UserID:        user.ID,
		AgentID:       agent.ID,
		Scope:         "session",
		CredentialRef: vaultKey,
		Service:       "google.gmail:eric@example.com",
		Host:          "gmail.googleapis.com",
		HeaderName:    "authorization",
		Scheme:        "bearer",
		Status:        "active",
		ExpiresAt:     &expiresAt,
	}); err != nil {
		t.Fatalf("CreateCredentialAuthorization: %v", err)
	}
	placeholder, err := autovault.GeneratePlaceholder(autovault.PlaceholderPrefix("google.gmail:eric@example.com"))
	if err != nil {
		t.Fatalf("GeneratePlaceholder: %v", err)
	}
	if err := st.CreateRuntimePlaceholder(context.Background(), &store.RuntimePlaceholder{
		Placeholder:       placeholder,
		UserID:            user.ID,
		AgentID:           agent.ID,
		ServiceID:         "google.gmail:eric@example.com",
		VaultItemID:       "google.gmail:eric@example.com",
		CredentialGrantID: grantID,
		ExpiresAt:         &expiresAt,
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/gmail/v1/users/me/messages", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "gmail.googleapis.com")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "OAUTH_CREDENTIAL_INVALID") {
		t.Fatalf("expected OAUTH_CREDENTIAL_INVALID in body, got %q", rec.Body.String())
	}
	if refreshRequests != 0 {
		t.Fatalf("expected no refresh attempt on unparseable credential, got %d", refreshRequests)
	}
	if upstreamCalls != 0 {
		t.Fatalf("stale access_token must not be forwarded upstream, got %d calls", upstreamCalls)
	}
}

// An explicit port on X-Clawvisor-Target-Host must pass the bound-service
// allowlist check (allowlist entries are hostnames) without losing the
// port for the actual upstream dial. Before the fix, the boundary check
// compared "api.github.com:8443" against ["api.github.com"] and rejected.
func TestResolver_AcceptsTargetHostWithExplicitPort(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/repos/x/y/issues", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com:8443")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with explicit port (allowlist should strip port for comparison), got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestResolver_RejectsMissingTargetHost(t *testing.T) {
	h, st, _, agent, nonces, placeholder := newSeededResolver(t)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/x", nil)
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 MISSING_TARGET, got %d", rec.Code)
	}
}

func TestProxyResolverTreatsCGNATAsPrivate(t *testing.T) {
	if !isPrivateIP(net.ParseIP("100.64.0.1")) {
		t.Fatal("expected RFC6598 CGNAT address to be treated as private")
	}
	if !isPrivateIP(net.ParseIP("100.127.255.254")) {
		t.Fatal("expected upper RFC6598 CGNAT address to be treated as private")
	}
	if isPrivateIP(net.ParseIP("100.128.0.1")) {
		t.Fatal("address outside RFC6598 CGNAT range should not be treated as private")
	}
}

func TestResolver_RejectsSelfTarget(t *testing.T) {
	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.SelfHostnames = []string{"clawvisor.example"}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/x", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "clawvisor.example")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 SELF_TARGET, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestResolver_RejectsForeignPlaceholder(t *testing.T) {
	h, st, _, agent, nonces, _ := newSeededResolver(t)

	// Mint a different placeholder owned by a different agent. The resolver
	// must refuse.
	other, err := st.CreateUser(context.Background(), "other@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	otherAgent, err := st.CreateAgent(context.Background(), other.ID, "other", "other-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	foreign, err := autovault.GeneratePlaceholder(autovault.PlaceholderPrefix("github"))
	if err != nil {
		t.Fatalf("GeneratePlaceholder: %v", err)
	}
	if err := st.CreateRuntimePlaceholder(context.Background(), &store.RuntimePlaceholder{
		Placeholder: foreign,
		UserID:      other.ID,
		AgentID:     otherAgent.ID,
		ServiceID:   "github",
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/x", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("Authorization", "Bearer "+foreign)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 PLACEHOLDER_OWNERSHIP, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestResolver_RejectsUnknownServicePlaceholderToArbitraryHost(t *testing.T) {
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h, st, user, agent, nonces, _ := newSeededResolver(t)
	const serviceID = "custom_api_key"
	if err := h.Vault.Set(context.Background(), user.ID, serviceID, []byte("custom-secret")); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}
	placeholder, err := autovault.GeneratePlaceholder(autovault.PlaceholderPrefix(serviceID))
	if err != nil {
		t.Fatalf("GeneratePlaceholder: %v", err)
	}
	if err := st.CreateRuntimePlaceholder(context.Background(), &store.RuntimePlaceholder{
		Placeholder: placeholder,
		UserID:      user.ID,
		AgentID:     agent.ID,
		ServiceID:   serviceID,
		VaultItemID: serviceID,
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/collect", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "attacker.example")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected unknown-service placeholder to fail closed, got %d (%s)", rec.Code, rec.Body.String())
	}
	if seenAuth != "" {
		t.Fatalf("unknown-service placeholder should not be swapped to arbitrary host, saw auth %q", seenAuth)
	}
}

func TestResolver_RejectsCallWithoutPlaceholder(t *testing.T) {
	h, st, _, agent, nonces, _ := newSeededResolver(t)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/x", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	// No header carries an autovault placeholder.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 NO_PLACEHOLDER, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestResolver_RejectsHostOutsideBoundService(t *testing.T) {
	// Placeholder is bound to "github" service, but caller asks resolver
	// to forward to slack.com — the bound-service host check refuses.
	h, st, _, agent, nonces, placeholder := newSeededResolver(t)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/api.test/path", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "slack.com")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 TARGET_HOST_NOT_BOUND, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "TARGET_HOST_NOT_BOUND") {
		t.Fatalf("expected TARGET_HOST_NOT_BOUND code, got %s", rec.Body.String())
	}
}

func TestResolver_AllowsUnknownServiceHostBoundByCredentialGrant(t *testing.T) {
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h, st, user, agent, nonces, _ := newSeededResolver(t)
	v := h.Vault.(*stubVault)
	if err := v.Set(context.Background(), user.ID, "agentphone", []byte("real-agentphone-token")); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}

	placeholder, err := autovault.GeneratePlaceholder(autovault.PlaceholderPrefix("agentphone"))
	if err != nil {
		t.Fatalf("GeneratePlaceholder: %v", err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	const grantID = "grant-agentphone"
	if err := st.CreateCredentialAuthorization(context.Background(), &store.CredentialAuthorization{
		ID:            grantID,
		UserID:        user.ID,
		AgentID:       agent.ID,
		Scope:         "session",
		CredentialRef: "agentphone",
		Service:       "agentphone",
		Host:          "api.agentphone.ai",
		HeaderName:    "authorization",
		Scheme:        "bearer",
		Status:        "active",
		ExpiresAt:     &expiresAt,
	}); err != nil {
		t.Fatalf("CreateCredentialAuthorization: %v", err)
	}
	if err := st.CreateRuntimePlaceholder(context.Background(), &store.RuntimePlaceholder{
		Placeholder:       placeholder,
		UserID:            user.ID,
		AgentID:           agent.ID,
		ServiceID:         "agentphone",
		VaultItemID:       "agentphone",
		CredentialGrantID: grantID,
		ExpiresAt:         &expiresAt,
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}

	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodPost, "/api/proxy/v1/calls", strings.NewReader(`{"to":"+15555550123"}`))
	req.Header.Set("X-Clawvisor-Target-Host", "api.agentphone.ai")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown service host, got %d (%s)", rec.Code, rec.Body.String())
	}
	if seenAuth != "Bearer real-agentphone-token" {
		t.Fatalf("expected upstream Authorization=Bearer real-agentphone-token, got %q", seenAuth)
	}
}

func TestResolver_StripsXClawvisorPrefixOnOutbound(t *testing.T) {
	var seenHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/x", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("X-Clawvisor-Custom", "secret")
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	for name := range seenHeaders {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Clawvisor-") {
			t.Fatalf("X-Clawvisor-* header leaked to upstream: %s", name)
		}
	}
}

// TestResolver_StripsForwardedHeadersOnOutbound ensures the resolver
// never propagates source-IP / vhost metadata an attacker — including
// the model itself, via tool_use header values — could set. Some
// upstreams trust X-Forwarded-For for IP allowlists and X-Forwarded-Host
// for vhost routing; passing them through makes the resolver an
// arbitrary-header injection vector.
func TestResolver_StripsForwardedHeadersOnOutbound(t *testing.T) {
	seen := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Forwarded-Ssl", "X-Real-Ip", "Forwarded", "Via"} {
			if v := r.Header.Get(name); v != "" {
				seen[name] = v
			}
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/x", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	// Attacker-influenced / proxy-injected forwarded headers.
	req.Header.Set("X-Forwarded-For", "10.0.0.1, attacker.example")
	req.Header.Set("X-Forwarded-Host", "internal.admin.example")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("X-Forwarded-Port", "1337")
	req.Header.Set("X-Forwarded-Ssl", "off")
	req.Header.Set("X-Real-IP", "10.0.0.1")
	req.Header.Set("Forwarded", "for=10.0.0.1;host=internal.admin.example")
	req.Header.Set("Via", "1.1 attacker")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(seen) != 0 {
		t.Fatalf("forwarded headers leaked to upstream: %v", seen)
	}
}

func TestResolver_StripsCallerAuthFromOutbound(t *testing.T) {
	// Even when a harness misuses Authorization to carry the caller token,
	// the resolver detects the cvis_ shape and strips it before forwarding.
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/x", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	// Placeholder rides on X-API-Key; Authorization carries a literal
	// cvis_-shaped token (a misconfigured client sending the agent
	// token where the proxy doesn't want it). Resolver should strip
	// Authorization rather than forward it upstream.
	req.Header.Set("Authorization", "Bearer cvis_should_not_leak_upstream")
	req.Header.Set("X-API-Key", placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(seenAuth, "cvis_") {
		t.Fatalf("caller token leaked to upstream Authorization: %q", seenAuth)
	}
}

// Regression: isSelfHost must strip a :port suffix before comparing.
// Without the strip, `clawvisor.example:443` slipped past `EqualFold`
// against the configured `clawvisor.example` and the resolver would
// happily forward through itself.
func TestResolver_RejectsSelfTargetWithPort(t *testing.T) {
	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.SelfHostnames = []string{"clawvisor.example"}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	for _, target := range []string{"clawvisor.example:443", "clawvisor.example:8080", "Clawvisor.Example:443"} {
		req := httptest.NewRequest(http.MethodGet, "/api/proxy/x", nil)
		// Set Target-Host BEFORE minting the nonce so the nonce is bound
		// to the target the resolver will see. Without this order, the
		// middleware's nonce-target check fires first and the 403 below
		// would arrive for the wrong reason — masking a real self-target
		// regression in the resolver.
		req.Header.Set("X-Clawvisor-Target-Host", target)
		req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
		req.Header.Set("Authorization", "Bearer "+placeholder)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("target %q: expected 403, got %d (%s)", target, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "SELF_TARGET") {
			t.Fatalf("target %q: expected SELF_TARGET error code in body, got %s", target, rec.Body.String())
		}
	}
}

// Security: the outbound client must NOT follow redirects automatically.
// A 3xx from the upstream would otherwise replay the swapped vault
// credential at the redirect target, bypassing the bound-service host
// allowlist and SSRF preflight.
func TestResolver_DoesNotFollowRedirects(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://evil.example.com/")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()

	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, nil, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/x", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	req.Header.Set("X-Clawvisor-Caller", nonceForRequest(t, nonces, agent.ID, req))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// The resolver must surface the 302 to the caller verbatim — not
	// follow it. The status code arriving at the client is the 302.
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 (no auto-follow), got %d (%s)", rec.Code, rec.Body.String())
	}
}

// Regression: the dial-time SSRF check must refuse private IPs even
// when the original DNS resolution at request-build time happened to
// return a public address (DNS rebinding TOCTOU). Direct exercise of
// safeDialContext rather than the full HTTP path so we don't need to
// run a private DNS server in the test.
func TestResolver_SafeDialContextRefusesPrivateIP(t *testing.T) {
	h, _, _, _, _, _ := newSeededResolver(t)
	h.AllowPrivateNetworks = false

	cases := []string{"127.0.0.1:80", "10.0.0.5:443", "192.168.1.1:8080", "169.254.169.254:80"}
	for _, addr := range cases {
		_, err := h.safeDialContext(context.Background(), "tcp", addr)
		if err == nil {
			t.Fatalf("expected dial blocked for %s", addr)
		}
		if !strings.Contains(err.Error(), "private IP") {
			t.Fatalf("expected 'private IP' in error for %s, got %v", addr, err)
		}
	}
}

// Sanity: when AllowPrivateNetworks=true, the dialer no longer blocks.
// (We don't actually dial; we just verify the early-return path doesn't
// fail with a "private IP" error.) The actual dial would still fail
// because nothing's listening, so we accept any error other than the
// private-IP block.
func TestResolver_SafeDialContextAllowsPrivateWhenFlagSet(t *testing.T) {
	h, _, _, _, _, _ := newSeededResolver(t)
	h.AllowPrivateNetworks = true
	_, err := h.safeDialContext(context.Background(), "tcp", "127.0.0.1:1") // unlikely listener
	if err != nil && strings.Contains(err.Error(), "private IP") {
		t.Fatalf("AllowPrivateNetworks should bypass private-IP block, got %v", err)
	}
}

// redirectTargetTransport rewrites every outbound URL to point at base.
// Lets the resolver dial the local httptest server even though it's told
// to reach a different X-Clawvisor-Target-Host.
type redirectTargetTransport struct {
	base string
}

func (rt *redirectTargetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(rt.base, "http://")
	return http.DefaultTransport.RoundTrip(clone)
}
