package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func newSeededAgent(t *testing.T) (store.Store, *store.Agent, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "agent-llm@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	raw, err := auth.GenerateAgentToken()
	if err != nil {
		t.Fatalf("GenerateAgentToken: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "test", auth.HashToken(raw))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return st, agent, raw
}

func newExpiredSeededAgent(t *testing.T) (store.Store, *store.Agent, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "expired-agent-llm@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	raw, err := auth.GenerateAgentToken()
	if err != nil {
		t.Fatalf("GenerateAgentToken: %v", err)
	}
	agent, err := st.CreateAgentWithExpiry(ctx, user.ID, "expired", auth.HashToken(raw), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("CreateAgentWithExpiry: %v", err)
	}
	return st, agent, raw
}

func TestRequireAgentLLM_AcceptsAuthorizationBearer(t *testing.T) {
	st, agent, raw := newSeededAgent(t)

	var seenAgent *store.Agent
	handler := RequireAgentLLM(st)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAgent = AgentFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if seenAgent == nil || seenAgent.ID != agent.ID {
		t.Fatalf("expected agent in context")
	}
}

func TestRequireAgentLLM_AcceptsXAPIKey(t *testing.T) {
	st, agent, raw := newSeededAgent(t)

	var seenAgent *store.Agent
	handler := RequireAgentLLM(st)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAgent = AgentFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	// Anthropic SDK convention: bearer in x-api-key, no Authorization.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with x-api-key auth, got %d (%s)", rec.Code, rec.Body.String())
	}
	if seenAgent == nil || seenAgent.ID != agent.ID {
		t.Fatalf("expected agent in context")
	}
}

func TestRequireAgentLLM_AcceptsClawvisorAgentTokenHeaderForPassthrough(t *testing.T) {
	st, agent, raw := newSeededAgent(t)

	var seenAgent *store.Agent
	var passthrough bool
	handler := RequireAgentLLM(st)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAgent = AgentFromContext(r.Context())
		passthrough = llmproxy.PassthroughUpstreamAuth(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set(AgentTokenHeader, raw)
	req.Header.Set("Authorization", "Bearer claude-oauth-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with %s auth, got %d (%s)", AgentTokenHeader, rec.Code, rec.Body.String())
	}
	if seenAgent == nil || seenAgent.ID != agent.ID {
		t.Fatalf("expected agent in context")
	}
	if !passthrough {
		t.Fatalf("expected passthrough upstream auth context")
	}
}

func TestRequireAgentLLM_RejectsExpiredAgentToken(t *testing.T) {
	st, _, raw := newExpiredSeededAgent(t)

	handler := RequireAgentLLM(st)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not run for an expired agent token")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired agent token, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestRequireAgentLLMNonce_RejectsExpiredNonceBoundAgent(t *testing.T) {
	st, agent, _ := newExpiredSeededAgent(t)
	nonces := llmproxy.NewMemoryCallerNonceCache(5 * time.Minute)
	nonce, err := nonces.Mint(context.Background(), agent.ID, llmproxy.NonceTarget{
		Host:   "api.github.com",
		Method: http.MethodPost,
		Path:   "/user",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	handler := RequireAgentLLMNonce(st, nonces, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not run for an expired nonce-bound agent")
	}))

	req := httptest.NewRequest(http.MethodPost, "/proxy/v1/user", nil)
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+nonce)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired nonce-bound agent, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestRequireAgentLLMNonce_AcceptsExplicitPortInTargetHostHeader is a
// regression test for the host:port asymmetry between the mint side
// (verdict.Host = url.URL.Hostname(), no port) and the rewriter
// (X-Clawvisor-Target-Host = parsed.Host, port preserved). Without the
// port-stripping normalization in the consume path, a non-default
// upstream port like https://api.example.com:8443 would round-trip
// from rewriter to resolver and fail nonce validation as
// NONCE_TARGET_MISMATCH.
func TestRequireAgentLLMNonce_AcceptsExplicitPortInTargetHostHeader(t *testing.T) {
	st, agent, _ := newSeededAgent(t)
	nonces := llmproxy.NewMemoryCallerNonceCache(5 * time.Minute)
	// Mint with bare hostname (matches inspector verdict).
	nonce, err := nonces.Mint(context.Background(), agent.ID, llmproxy.NonceTarget{
		Host:   "api.example.com",
		Method: http.MethodGet,
		Path:   "/v1/whoami",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	handlerCalled := false
	handler := RequireAgentLLMNonce(st, nonces, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/v1/whoami", nil)
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+nonce)
	// Rewriter emits host:port verbatim from parsed.Host.
	req.Header.Set("X-Clawvisor-Target-Host", "api.example.com:8443")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with port-stripped host match, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !handlerCalled {
		t.Fatal("downstream handler was not invoked")
	}
}

func TestRequireAgentLLM_RejectsMissingOrInvalid(t *testing.T) {
	st, _, _ := newSeededAgent(t)

	handler := RequireAgentLLM(st)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not run")
	}))

	cases := []struct {
		name, header, value string
	}{
		{"missing", "", ""},
		{"non-cvis Bearer", "Authorization", "Bearer some-other-token"},
		{"non-cvis x-api-key", "x-api-key", "sk-ant-real-key"},
		{"bogus cvis", "Authorization", "Bearer cvis_nonsense"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if tc.header != "" {
				req.Header.Set(tc.header, tc.value)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", rec.Code)
			}
		})
	}
}
