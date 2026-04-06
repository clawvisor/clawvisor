package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
	sqlitestore "github.com/clawvisor/clawvisor/internal/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/internal/vault"
	"github.com/clawvisor/clawvisor/pkg/vault"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ── Test environment with hooks ──────────────────────────────────────────────

// newTestEnvWithHooks creates a testEnv with GatewayHooks and optional extra adapters.
func newTestEnvWithHooks(t *testing.T, hooks *api.GatewayHooks, extra ...adapters.Adapter) *testEnv {
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

	adapterReg := adapters.NewRegistry()
	for _, a := range extra {
		adapterReg.Register(a)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0},
		Auth: config.AuthConfig{
			JWTSecret:       "test-secret-for-integration-tests",
			AccessTokenTTL:  "15m",
			RefreshTokenTTL: "720h",
		},
		Approval: config.ApprovalConfig{Timeout: 300, OnTimeout: "fail"},
		Task:     config.TaskConfig{DefaultExpirySeconds: 3600},
	}

	opts := []api.ServerOption{
		api.WithFeatures(api.FeatureSet{PasswordAuth: true}),
	}
	if hooks != nil {
		opts = append(opts, api.WithGatewayHooks(hooks))
	}

	srv, err := api.New(cfg, st, v, jwtSvc, adapterReg, nil, config.LLMConfig{}, nil, opts...)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = st.Close() })

	return &testEnv{
		t:      t,
		ts:     ts,
		Vault:  v,
		Store:  st,
		client: ts.Client(),
	}
}

// ── Agent OrgID persistence ──────────────────────────────────────────────────

func TestAgentOrgID_CreateAgentWithOrg(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Create agent with org via store directly (simulates cloud's agent creation).
	tokenHash := "test-hash-org-agent"
	agent, err := env.Store.CreateAgentWithOrg(context.Background(), s.UserID, "org-agent", tokenHash, "org-123")
	if err != nil {
		t.Fatalf("CreateAgentWithOrg: %v", err)
	}
	if agent.OrgID != "org-123" {
		t.Errorf("expected OrgID %q, got %q", "org-123", agent.OrgID)
	}
	if agent.Name != "org-agent" {
		t.Errorf("expected Name %q, got %q", "org-agent", agent.Name)
	}
}

func TestAgentOrgID_RegularAgentHasEmptyOrgID(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Create agent via API (no org context — public flow).
	resp := s.do("POST", "/api/agents", map[string]any{"name": "no-org-agent"})
	body := mustStatus(t, resp, http.StatusCreated)
	agentID := str(t, body, "id")

	// Fetch the agent from the store directly.
	agents, err := env.Store.ListAgents(context.Background(), s.UserID)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	var found *store.Agent
	for _, a := range agents {
		if a.ID == agentID {
			found = a
			break
		}
	}
	if found == nil {
		t.Fatal("agent not found in store")
	}
	if found.OrgID != "" {
		t.Errorf("expected empty OrgID for API-created agent, got %q", found.OrgID)
	}
}

func TestAgentOrgID_GetAgentByTokenReturnsOrgID(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	tokenHash := "test-hash-with-org"
	_, err := env.Store.CreateAgentWithOrg(context.Background(), s.UserID, "org-agent", tokenHash, "org-456")
	if err != nil {
		t.Fatalf("CreateAgentWithOrg: %v", err)
	}

	// Retrieve by token hash.
	agent, err := env.Store.GetAgentByToken(context.Background(), tokenHash)
	if err != nil {
		t.Fatalf("GetAgentByToken: %v", err)
	}
	if agent.OrgID != "org-456" {
		t.Errorf("expected OrgID %q from GetAgentByToken, got %q", "org-456", agent.OrgID)
	}
}

func TestAgentOrgID_ListAgentsIncludesOrgID(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Create one agent with org and one without.
	_, err := env.Store.CreateAgentWithOrg(context.Background(), s.UserID, "org-agent", "hash-a", "org-789")
	if err != nil {
		t.Fatalf("CreateAgentWithOrg: %v", err)
	}

	resp := s.do("POST", "/api/agents", map[string]any{"name": "plain-agent"})
	mustStatus(t, resp, http.StatusCreated)

	agents, err := env.Store.ListAgents(context.Background(), s.UserID)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	var orgAgent, plainAgent *store.Agent
	for _, a := range agents {
		switch a.Name {
		case "org-agent":
			orgAgent = a
		case "plain-agent":
			plainAgent = a
		}
	}

	if orgAgent == nil {
		t.Fatal("org-agent not found")
	}
	if orgAgent.OrgID != "org-789" {
		t.Errorf("org-agent OrgID: expected %q, got %q", "org-789", orgAgent.OrgID)
	}

	if plainAgent == nil {
		t.Fatal("plain-agent not found")
	}
	if plainAgent.OrgID != "" {
		t.Errorf("plain-agent OrgID: expected empty, got %q", plainAgent.OrgID)
	}
}

// ── GatewayHooks BeforeAuthorize ────────────────────────────────────────────

func TestGatewayHooks_BeforeAuthorizeBlocks(t *testing.T) {
	mock := newMockAdapter("test.svc", "do_thing").
		withResult("done", map[string]any{"ok": true})

	hooks := &api.GatewayHooks{
		BeforeAuthorize: func(ctx context.Context, agentID, userID, service, action string) error {
			if service == "test.svc" && action == "do_thing" {
				return fmt.Errorf("blocked by org policy: test.svc:do_thing is restricted")
			}
			return nil
		},
	}

	env := newTestEnvWithHooks(t, hooks, mock)
	sc := newScenario(t, env, "")
	taskID := sc.createApprovedTask(t, env, "test.svc", "do_thing", true)

	result := sc.gatewayRequestWithTask(env, "req-blocked-1", "test.svc", "do_thing", taskID)

	status, _ := result["status"].(string)
	if status != "blocked" {
		t.Errorf("expected status %q, got %q (full response: %v)", "blocked", status, result)
	}

	reason, _ := result["reason"].(string)
	strContains(t, reason, "org policy", "block reason")
}

func TestGatewayHooks_BeforeAuthorizeAllows(t *testing.T) {
	mock := newMockAdapter("test.svc", "do_thing").
		withResult("done", map[string]any{"ok": true})

	hooks := &api.GatewayHooks{
		BeforeAuthorize: func(ctx context.Context, agentID, userID, service, action string) error {
			// Allow everything.
			return nil
		},
	}

	env := newTestEnvWithHooks(t, hooks, mock)
	sc := newScenario(t, env, "")
	taskID := sc.createApprovedTask(t, env, "test.svc", "do_thing", true)

	result := sc.gatewayRequestWithTask(env, "req-allowed-1", "test.svc", "do_thing", taskID)

	status, _ := result["status"].(string)
	if status != "executed" {
		t.Errorf("expected status %q, got %q (full response: %v)", "executed", status, result)
	}
}

func TestGatewayHooks_NilHooksDoesNotBlock(t *testing.T) {
	// Verify that without hooks, gateway works normally.
	mock := newMockAdapter("test.svc", "do_thing").
		withResult("done", map[string]any{"ok": true})

	env := newTestEnvWithHooks(t, nil, mock)
	sc := newScenario(t, env, "")
	taskID := sc.createApprovedTask(t, env, "test.svc", "do_thing", true)

	result := sc.gatewayRequestWithTask(env, "req-nohook-1", "test.svc", "do_thing", taskID)

	status, _ := result["status"].(string)
	if status != "executed" {
		t.Errorf("expected status %q, got %q (full response: %v)", "executed", status, result)
	}
}

func TestGatewayHooks_BeforeAuthorizeReceivesCorrectArgs(t *testing.T) {
	mock := newMockAdapter("test.svc", "my_action").
		withResult("done", nil)

	var capturedAgentID, capturedUserID, capturedService, capturedAction string

	hooks := &api.GatewayHooks{
		BeforeAuthorize: func(ctx context.Context, agentID, userID, service, action string) error {
			capturedAgentID = agentID
			capturedUserID = userID
			capturedService = service
			capturedAction = action
			return nil
		},
	}

	env := newTestEnvWithHooks(t, hooks, mock)
	sc := newScenario(t, env, "")
	taskID := sc.createApprovedTask(t, env, "test.svc", "my_action", true)

	sc.gatewayRequestWithTask(env, "req-args-1", "test.svc", "my_action", taskID)

	if capturedAgentID != sc.AgentID {
		t.Errorf("agentID: expected %q, got %q", sc.AgentID, capturedAgentID)
	}
	if capturedUserID != sc.session.UserID {
		t.Errorf("userID: expected %q, got %q", sc.session.UserID, capturedUserID)
	}
	if capturedService != "test.svc" {
		t.Errorf("service: expected %q, got %q", "test.svc", capturedService)
	}
	if capturedAction != "my_action" {
		t.Errorf("action: expected %q, got %q", "my_action", capturedAction)
	}
}

// ── Registry fallback (GetWithContext) ──────────────────────────────────────

func TestRegistryGetWithContext_BuiltInTakesPrecedence(t *testing.T) {
	reg := adapters.NewRegistry()
	mock := newMockAdapter("builtin.svc", "action")
	reg.Register(mock)

	// Set a fallback that would return a different adapter.
	reg.SetFallback(func(ctx context.Context, serviceID string) (adapters.Adapter, bool) {
		return newMockAdapter("fallback.svc", "other"), true
	})

	a, ok := reg.GetWithContext(context.Background(), "builtin.svc")
	if !ok {
		t.Fatal("expected to find builtin.svc")
	}
	if a.ServiceID() != "builtin.svc" {
		t.Errorf("expected builtin.svc, got %s", a.ServiceID())
	}
}

func TestRegistryGetWithContext_FallbackResolves(t *testing.T) {
	reg := adapters.NewRegistry()

	fallbackMock := newMockAdapter("custom.internal", "call")
	reg.SetFallback(func(ctx context.Context, serviceID string) (adapters.Adapter, bool) {
		if serviceID == "custom.internal" {
			return fallbackMock, true
		}
		return nil, false
	})

	a, ok := reg.GetWithContext(context.Background(), "custom.internal")
	if !ok {
		t.Fatal("expected fallback to resolve custom.internal")
	}
	if a.ServiceID() != "custom.internal" {
		t.Errorf("expected custom.internal, got %s", a.ServiceID())
	}
}

func TestRegistryGetWithContext_NoFallbackReturnsNotFound(t *testing.T) {
	reg := adapters.NewRegistry()

	_, ok := reg.GetWithContext(context.Background(), "nonexistent.svc")
	if ok {
		t.Error("expected not found without fallback")
	}
}

func TestRegistryGetWithContext_FallbackReturnsFalse(t *testing.T) {
	reg := adapters.NewRegistry()
	reg.SetFallback(func(ctx context.Context, serviceID string) (adapters.Adapter, bool) {
		return nil, false
	})

	_, ok := reg.GetWithContext(context.Background(), "nonexistent.svc")
	if ok {
		t.Error("expected not found when fallback returns false")
	}
}

// ── Vault wrapper integration pattern test ──────────────────────────────────

// TestVaultFallbackPattern verifies that the vault.Vault interface allows
// wrapping (the pattern cloud uses for org-aware credential resolution).
func TestVaultFallbackPattern(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// This test verifies the wrapping pattern works: a wrapper vault
	// can fall through to the underlying vault.
	wrapper := &fallbackVault{primary: env.Vault}

	ctx := context.Background()
	userID := s.UserID
	service := "test.svc"

	// Set a credential in the primary vault.
	if err := env.Vault.Set(ctx, userID, service, []byte("real-cred")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The wrapper should fall through to the primary.
	cred, err := wrapper.Get(ctx, userID, service)
	if err != nil {
		t.Fatalf("wrapper.Get: %v", err)
	}
	if string(cred) != "real-cred" {
		t.Errorf("expected %q, got %q", "real-cred", string(cred))
	}
}

// fallbackVault is a minimal vault wrapper that demonstrates the pattern
// cloud uses for OrgAwareVault (org vault → user vault fallback).
type fallbackVault struct {
	primary vault.Vault
}

func (v *fallbackVault) Get(ctx context.Context, userID, service string) ([]byte, error) {
	// In the real implementation, this would check org vault first.
	return v.primary.Get(ctx, userID, service)
}

func (v *fallbackVault) Set(ctx context.Context, userID, service string, cred []byte) error {
	return v.primary.Set(ctx, userID, service, cred)
}

func (v *fallbackVault) Delete(ctx context.Context, userID, service string) error {
	return v.primary.Delete(ctx, userID, service)
}

func (v *fallbackVault) List(ctx context.Context, userID string) ([]string, error) {
	return v.primary.List(ctx, userID)
}
