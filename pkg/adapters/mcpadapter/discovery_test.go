package mcpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/mcpadapter"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// memVault is a barebones in-memory Vault for tests in this package — only
// the three methods MCPAdapter actually touches are implemented.
type memVault struct {
	creds map[string]map[string][]byte
}

func newMemVault() *memVault { return &memVault{creds: map[string]map[string][]byte{}} }

func (m *memVault) Set(_ context.Context, userID, key string, c []byte) error {
	if m.creds[userID] == nil {
		m.creds[userID] = map[string][]byte{}
	}
	m.creds[userID][key] = c
	return nil
}
func (m *memVault) SetIfAbsent(_ context.Context, userID, key string, c []byte) error {
	if m.creds[userID] == nil {
		m.creds[userID] = map[string][]byte{}
	}
	if _, ok := m.creds[userID][key]; ok {
		return vault.ErrAlreadyExists
	}
	m.creds[userID][key] = c
	return nil
}
func (m *memVault) Get(_ context.Context, userID, key string) ([]byte, error) {
	if u, ok := m.creds[userID]; ok {
		if c, ok := u[key]; ok {
			return c, nil
		}
	}
	return nil, vault.ErrNotFound
}
func (m *memVault) Delete(_ context.Context, userID, key string) error {
	if u, ok := m.creds[userID]; ok {
		delete(u, key)
	}
	return nil
}
func (m *memVault) List(_ context.Context, userID string) ([]string, error) {
	out := []string{}
	for k := range m.creds[userID] {
		out = append(out, k)
	}
	return out, nil
}

// discoveryFixture spins up an httptest.Server that emulates Notion's MCP
// discovery + registration surface, with counters so tests can assert
// re-registration didn't fire.
type discoveryFixture struct {
	srv             *httptest.Server
	registrations   atomic.Int64
	mcpProbes       atomic.Int64
	resourceProbes  atomic.Int64
	asProbes        atomic.Int64
	rejectRegistration bool // for the failure-mode test
	authMethodsOnly    []string // override token_endpoint_auth_methods_supported (nil = default)
}

func newDiscoveryFixture(t *testing.T) *discoveryFixture {
	t.Helper()
	f := &discoveryFixture{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		f.mcpProbes.Add(1)
		// Build the resource_metadata URL from the test server's base URL
		// so probes don't escape to the live internet.
		base := "http://" + r.Host
		w.Header().Set("Www-Authenticate", `Bearer realm="OAuth", resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp", error="invalid_token"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		f.resourceProbes.Add(1)
		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              base,
			"authorization_servers": []string{base},
			"bearer_methods_supported": []string{"header"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		f.asProbes.Add(1)
		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		authMethods := f.authMethodsOnly
		if authMethods == nil {
			authMethods = []string{"client_secret_post", "client_secret_basic", "none"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/authorize",
			"token_endpoint":                        base + "/token",
			"registration_endpoint":                 base + "/register",
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"response_types_supported":              []string{"code"},
			"token_endpoint_auth_methods_supported": authMethods,
			"code_challenge_methods_supported":      []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		f.registrations.Add(1)
		if f.rejectRegistration {
			http.Error(w, `{"error":"invalid_client_metadata"}`, http.StatusBadRequest)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Validate the request body shape — caller MUST send redirect_uris
		// matching Clawvisor's OAuth callback so the registered client_id
		// is bound to the right URL.
		uris, _ := body["redirect_uris"].([]any)
		if len(uris) == 0 || uris[0] != "http://localhost:25297/api/oauth/callback" {
			http.Error(w, `{"error":"invalid_redirect_uri"}`, http.StatusBadRequest)
			return
		}
		method, _ := body["token_endpoint_auth_method"].(string)
		var secret string
		if method != "none" {
			secret = "registered-secret-xyz"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":     "registered-client-abc",
			"client_secret": secret,
			"redirect_uris": uris,
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *discoveryFixture) httpClient() *http.Client {
	// Force every outbound URL through the test server's transport so
	// probes don't accidentally escape to the real internet.
	parsed, _ := url.Parse(f.srv.URL)
	return &http.Client{
		Transport: &redirectTransport{base: parsed},
		Timeout:   f.srv.Client().Timeout,
	}
}

// redirectTransport rewrites the URL host:port of every request to point at
// the httptest.Server. Lets us probe what looks like a Notion URL but
// actually hits the test server.
type redirectTransport struct{ base *url.URL }

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = t.base.Scheme
	req.URL.Host = t.base.Host
	return http.DefaultTransport.RoundTrip(req)
}

// TestEnsureOAuthReady_DiscoversAndRegisters is the load-bearing test: a
// freshly-loaded MCP adapter with no cached credentials performs the full
// RFC 9728 → RFC 8414 → RFC 7591 dance and caches the result.
func TestEnsureOAuthReady_DiscoversAndRegisters(t *testing.T) {
	fx := newDiscoveryFixture(t)
	v := newMemVault()

	var spec mcpadapter.Spec
	spec.Service.ID = "test-mcp"
	spec.MCP.Transport = "http"
	spec.MCP.Endpoint = "https://mcp.example.com/mcp" // intercepted by redirectTransport
	spec.MCP.OAuth = &mcpadapter.MCPOAuthSpec{}       // marker only; no hardcoded endpoints

	adapter := mcpadapter.FromSpec(spec, &mcpadapter.HTTPTransport{Endpoint: spec.MCP.Endpoint})
	adapter.SetOAuthVault(v)
	adapter.SetDiscoveryHTTPClient(fx.httpClient())

	ctx := context.Background()
	if err := adapter.EnsureOAuthReady(ctx, "http://localhost:25297/api/oauth/callback"); err != nil {
		t.Fatalf("EnsureOAuthReady: %v", err)
	}

	// Assert all four phases ran exactly once.
	if got := fx.mcpProbes.Load(); got != 1 {
		t.Errorf("mcp probe count = %d, want 1", got)
	}
	if got := fx.resourceProbes.Load(); got != 1 {
		t.Errorf("resource metadata fetch count = %d, want 1", got)
	}
	if got := fx.asProbes.Load(); got != 1 {
		t.Errorf("authorization-server metadata fetch count = %d, want 1", got)
	}
	if got := fx.registrations.Load(); got != 1 {
		t.Errorf("registration count = %d, want 1", got)
	}

	// Client record persisted with discovered endpoints + registered client.
	rec := adapters.GetMCPClientRecord(ctx, v, "test-mcp")
	if rec == nil {
		t.Fatal("client record was not cached")
	}
	if rec.ClientID != "registered-client-abc" {
		t.Errorf("client_id = %q, want %q", rec.ClientID, "registered-client-abc")
	}
	if rec.ClientSecret != "registered-secret-xyz" {
		t.Errorf("client_secret missing — confidential client should have one")
	}
	if !strings.HasSuffix(rec.AuthorizationEndpoint, "/authorize") {
		t.Errorf("authorization_endpoint not captured: %q", rec.AuthorizationEndpoint)
	}
	if !strings.HasSuffix(rec.TokenEndpoint, "/token") {
		t.Errorf("token_endpoint not captured: %q", rec.TokenEndpoint)
	}

	// OAuthConfig now returns a populated config using discovered endpoints.
	cfg := adapter.OAuthConfig()
	if cfg == nil {
		t.Fatal("OAuthConfig should be non-nil after discovery")
	}
	if cfg.ClientID != "registered-client-abc" {
		t.Errorf("oauth config has wrong client_id: %q", cfg.ClientID)
	}
	if !strings.HasSuffix(cfg.Endpoint.AuthURL, "/authorize") {
		t.Errorf("oauth config has stale auth URL: %q", cfg.Endpoint.AuthURL)
	}

	// ── Idempotency: a second EnsureOAuthReady is a no-op. ──
	if err := adapter.EnsureOAuthReady(ctx, "http://localhost:25297/api/oauth/callback"); err != nil {
		t.Fatalf("second EnsureOAuthReady: %v", err)
	}
	if got := fx.registrations.Load(); got != 1 {
		t.Errorf("re-registration fired (count=%d) — cache miss", got)
	}
}

// TestEnsureOAuthReady_AdminPinSkipsDiscovery confirms that an admin-pinned
// override at mcp.oauth.{serviceID} bypasses dynamic registration entirely.
// Mirrors the path self-hosters take when they want a specific client.
func TestEnsureOAuthReady_AdminPinSkipsDiscovery(t *testing.T) {
	fx := newDiscoveryFixture(t)
	v := newMemVault()
	ctx := context.Background()

	// Admin pre-pinned a client.
	if err := adapters.SetMCPOAuthCredentials(ctx, v, "test-mcp", "pinned-cid", "pinned-csec"); err != nil {
		t.Fatalf("SetMCPOAuthCredentials: %v", err)
	}

	var spec mcpadapter.Spec
	spec.Service.ID = "test-mcp"
	spec.MCP.Transport = "http"
	spec.MCP.Endpoint = "https://mcp.example.com/mcp"
	spec.MCP.OAuth = &mcpadapter.MCPOAuthSpec{
		AuthorizeURL: "https://example.com/admin/authorize",
		TokenURL:     "https://example.com/admin/token",
	}

	adapter := mcpadapter.FromSpec(spec, &mcpadapter.HTTPTransport{Endpoint: spec.MCP.Endpoint})
	adapter.SetOAuthVault(v)
	adapter.SetDiscoveryHTTPClient(fx.httpClient())

	if err := adapter.EnsureOAuthReady(ctx, "http://localhost:25297/api/oauth/callback"); err != nil {
		t.Fatalf("EnsureOAuthReady: %v", err)
	}
	if got := fx.registrations.Load(); got != 0 {
		t.Errorf("admin pin should skip registration, but %d fired", got)
	}
	cfg := adapter.OAuthConfig()
	if cfg == nil || cfg.ClientID != "pinned-cid" {
		t.Fatalf("expected admin-pinned client, got %+v", cfg)
	}
}

// TestEnsureOAuthReady_RegistrationFailure surfaces the registration error
// up to the caller (the OAuth handler in services.go), so the UI gets a
// meaningful failure mode instead of a silent fallback.
func TestEnsureOAuthReady_RegistrationFailure(t *testing.T) {
	fx := newDiscoveryFixture(t)
	fx.rejectRegistration = true
	v := newMemVault()

	var spec mcpadapter.Spec
	spec.Service.ID = "test-mcp"
	spec.MCP.Transport = "http"
	spec.MCP.Endpoint = "https://mcp.example.com/mcp"
	spec.MCP.OAuth = &mcpadapter.MCPOAuthSpec{}

	adapter := mcpadapter.FromSpec(spec, &mcpadapter.HTTPTransport{Endpoint: spec.MCP.Endpoint})
	adapter.SetOAuthVault(v)
	adapter.SetDiscoveryHTTPClient(fx.httpClient())

	err := adapter.EnsureOAuthReady(context.Background(), "http://localhost:25297/api/oauth/callback")
	if err == nil {
		t.Fatal("expected error when registration is rejected")
	}
	if !strings.Contains(err.Error(), "register") {
		t.Errorf("expected error to mention 'register', got: %v", err)
	}
	// Nothing should be cached on failure — next attempt should re-try.
	if rec := adapters.GetMCPClientRecord(context.Background(), v, "test-mcp"); rec != nil {
		t.Errorf("client record should not be cached on failure, got %+v", rec)
	}
}

// TestEnsureOAuthReady_RefusesPublicClientOnly is the regression test for
// the public-client gap: the standard OAuth flow doesn't add PKCE
// parameters, so registering as a public client (token_endpoint_auth_method
// = "none") would produce a client we can't actually use. Refuse to
// register rather than getting a confusing 401 at token exchange.
func TestEnsureOAuthReady_RefusesPublicClientOnly(t *testing.T) {
	fx := newDiscoveryFixture(t)
	fx.authMethodsOnly = []string{"none"} // only public clients supported
	v := newMemVault()

	var spec mcpadapter.Spec
	spec.Service.ID = "public-only-mcp"
	spec.MCP.Transport = "http"
	spec.MCP.Endpoint = "https://mcp.example.com/mcp"
	spec.MCP.OAuth = &mcpadapter.MCPOAuthSpec{}

	adapter := mcpadapter.FromSpec(spec, &mcpadapter.HTTPTransport{Endpoint: spec.MCP.Endpoint})
	adapter.SetOAuthVault(v)
	adapter.SetDiscoveryHTTPClient(fx.httpClient())

	err := adapter.EnsureOAuthReady(context.Background(), "http://localhost:25297/api/oauth/callback")
	if err == nil {
		t.Fatal("expected error when AS supports only public clients")
	}
	if !strings.Contains(err.Error(), "public OAuth clients") {
		t.Errorf("expected error to mention public OAuth clients, got: %v", err)
	}
	// Importantly we must NOT have POSTed to /register — refusing to register
	// at all is the point of the fix.
	if got := fx.registrations.Load(); got != 0 {
		t.Errorf("expected 0 registration attempts, got %d", got)
	}
	// And nothing got cached.
	if rec := adapters.GetMCPClientRecord(context.Background(), v, "public-only-mcp"); rec != nil {
		t.Errorf("client record should not be cached on refusal, got %+v", rec)
	}
}
