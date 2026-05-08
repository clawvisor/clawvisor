package handlers

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func newSeededResolver(t *testing.T) (*ProxyResolverHandler, store.Store, *store.User, *store.Agent, string, string) {
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
	return h, st, user, agent, rawAgentToken, placeholder
}

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

	h, st, _, _, rawToken, placeholder := newSeededResolver(t)

	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMCaller(st)
	mux.Handle("/proxy/v1/", mw(http.HandlerFunc(h.Forward)))

	// Target host: api.github.com (in the github bound-service allowlist).
	// The redirectTargetTransport sends the actual dial to httptest's
	// loopback URL, but the resolver believes (and validates against)
	// api.github.com.
	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/repos/x/y/issues", strings.NewReader(""))
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+rawToken)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
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
	_ = seenBody
}

func TestResolver_RejectsMissingTargetHost(t *testing.T) {
	h, st, _, _, rawToken, placeholder := newSeededResolver(t)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMCaller(st)
	mux.Handle("/proxy/v1/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/x", nil)
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+rawToken)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 MISSING_TARGET, got %d", rec.Code)
	}
}

func TestResolver_RejectsSelfTarget(t *testing.T) {
	h, st, _, _, rawToken, placeholder := newSeededResolver(t)
	h.SelfHostnames = []string{"clawvisor.example"}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMCaller(st)
	mux.Handle("/proxy/v1/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/x", nil)
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+rawToken)
	req.Header.Set("X-Clawvisor-Target-Host", "clawvisor.example")
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 SELF_TARGET, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestResolver_RejectsForeignPlaceholder(t *testing.T) {
	h, st, _, _, rawToken, _ := newSeededResolver(t)

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
	mw := middleware.RequireAgentLLMCaller(st)
	mux.Handle("/proxy/v1/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/x", nil)
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+rawToken)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	req.Header.Set("Authorization", "Bearer "+foreign)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 PLACEHOLDER_OWNERSHIP, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestResolver_RejectsCallWithoutPlaceholder(t *testing.T) {
	h, st, _, _, rawToken, _ := newSeededResolver(t)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMCaller(st)
	mux.Handle("/proxy/v1/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/x", nil)
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+rawToken)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
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
	h, st, _, _, rawToken, placeholder := newSeededResolver(t)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMCaller(st)
	mux.Handle("/proxy/v1/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/api.test/path", nil)
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+rawToken)
	req.Header.Set("X-Clawvisor-Target-Host", "slack.com")
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

func TestResolver_StripsXClawvisorPrefixOnOutbound(t *testing.T) {
	var seenHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h, st, _, _, rawToken, placeholder := newSeededResolver(t)
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMCaller(st)
	mux.Handle("/proxy/v1/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/x", nil)
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+rawToken)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
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

func TestResolver_StripsCallerAuthFromOutbound(t *testing.T) {
	// Even when a harness misuses Authorization to carry the caller token,
	// the resolver detects the cvis_ shape and strips it before forwarding.
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h, st, _, _, rawToken, placeholder := newSeededResolver(t)
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMCaller(st)
	mux.Handle("/proxy/v1/", mw(http.HandlerFunc(h.Forward)))

	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/x", nil)
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+rawToken)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	// Placeholder rides on X-API-Key; Authorization carries cvis_ caller
	// token (a misconfiguration). Resolver should strip Authorization.
	req.Header.Set("Authorization", "Bearer "+rawToken)
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
	h, st, _, _, rawToken, placeholder := newSeededResolver(t)
	h.SelfHostnames = []string{"clawvisor.example"}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMCaller(st)
	mux.Handle("/proxy/v1/", mw(http.HandlerFunc(h.Forward)))

	for _, target := range []string{"clawvisor.example:443", "clawvisor.example:8080", "Clawvisor.Example:443"} {
		req := httptest.NewRequest(http.MethodGet, "/proxy/v1/x", nil)
		req.Header.Set("X-Clawvisor-Caller", "Bearer "+rawToken)
		req.Header.Set("X-Clawvisor-Target-Host", target)
		req.Header.Set("Authorization", "Bearer "+placeholder)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("target %q: expected 403, got %d (%s)", target, rec.Code, rec.Body.String())
		}
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
