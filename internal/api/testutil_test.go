package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/internal/api"
	"github.com/clawvisor/clawvisor/internal/auth"
	sqlitestore "github.com/clawvisor/clawvisor/internal/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/internal/vault"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// ── Test environment ──────────────────────────────────────────────────────────

// testEnv is a running httptest.Server wired to a real SQLite DB and vault.
// Create one per test with newTestEnv; it is cleaned up via t.Cleanup.
type testEnv struct {
	t      *testing.T
	ts     *httptest.Server
	Vault  vault.Vault
	Store  store.Store
	client *http.Client
}

// newTestEnv spins up a full API server backed by an in-process SQLite DB.
// Pass extra adapters to register them in the adapter registry (e.g. mockAdapter).
func newTestEnv(t *testing.T, extra ...adapters.Adapter) *testEnv {
	return newTestEnvWithLLM(t, config.LLMConfig{}, extra...)
}

func newTestEnvWithLLM(t *testing.T, llmCfg config.LLMConfig, extra ...adapters.Adapter) *testEnv {
	t.Helper()

	ctx := context.Background()

	// SQLite in a temp directory (t.Cleanup will remove it)
	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	st := sqlitestore.NewStore(db)

	// LocalVault with auto-generated key
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

	// Tests use password auth so Register/Login routes are available.
	srv, err := api.New(cfg, st, v, jwtSvc, adapterReg, nil, llmCfg, nil,
		api.WithFeatures(api.FeatureSet{PasswordAuth: true}),
	)
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

// magicLinkTestEnv extends testEnv with a MagicTokenStore for magic-link tests.
type magicLinkTestEnv struct {
	*testEnv
	magicStore *auth.MagicTokenStore
}

// newMagicLinkTestEnv spins up an API server with magic-link auth (PasswordAuth: false).
// Users must be created directly via Store.CreateUser since Register is unavailable.
func newMagicLinkTestEnv(t *testing.T) *magicLinkTestEnv {
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
		Approval: config.ApprovalConfig{Timeout: 300, OnTimeout: "fail"},
		Task:     config.TaskConfig{DefaultExpirySeconds: 3600},
	}

	ms := auth.NewMagicTokenStore()

	srv, err := api.New(cfg, st, v, jwtSvc, adapters.NewRegistry(), nil, config.LLMConfig{}, ms,
		api.WithFeatures(api.FeatureSet{PasswordAuth: false}),
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = st.Close() })

	return &magicLinkTestEnv{
		testEnv: &testEnv{
			t:      t,
			ts:     ts,
			Vault:  v,
			Store:  st,
			client: ts.Client(),
		},
		magicStore: ms,
	}
}

// createUser creates a user directly in the store (bypasses auth routes).
func (e *magicLinkTestEnv) createUser(t *testing.T) (userID, email string) {
	t.Helper()
	email = fmt.Sprintf("magic-%s@test.example", randSuffix())
	user, err := e.Store.CreateUser(context.Background(), email, "unused-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return user.ID, email
}

// url builds an absolute URL for the given path.
func (e *testEnv) url(path string) string {
	return e.ts.URL + path
}

// do performs an HTTP request, JSON-encoding body if non-nil.
func (e *testEnv) do(method, path string, token string, body any) *http.Response {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.url(path), r)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// decode reads and JSON-decodes the response body into v.
// It always drains and closes the body.
func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if v != nil {
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("decode JSON (status=%d body=%s): %v", resp.StatusCode, b, err)
		}
	}
}

// mustStatus asserts the response has the expected HTTP status code,
// decodes the body into a map (if any), and returns it.
// Responses with no body (e.g. 204) return a nil map without error.
func mustStatus(t *testing.T, resp *http.Response, want int) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != want {
		t.Fatalf("expected HTTP %d, got %d: %s", want, resp.StatusCode, b)
	}
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode JSON (status=%d body=%s): %v", resp.StatusCode, b, err)
	}
	return m
}

// str extracts a string field from a decoded map.
func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q not found in %v", key, m)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("key %q is not a string (got %T: %v)", key, v, v)
	}
	return s
}

// nested extracts a nested map from a decoded map.
func nested(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q not found in %v", key, m)
	}
	n, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("key %q is not an object (got %T)", key, v)
	}
	return n
}

// arr extracts a slice from a decoded map.
func arr(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q not found in %v", key, m)
	}
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("key %q is not an array (got %T)", key, v)
	}
	return a
}

// ── Test session: registers + logs in a user ──────────────────────────────────

type testSession struct {
	env          *testEnv
	Email        string
	UserID       string
	AccessToken  string
	RefreshToken string
}

// newSession registers a fresh user and returns a session with tokens.
func newSession(t *testing.T, env *testEnv) *testSession {
	t.Helper()
	email := fmt.Sprintf("user-%s@test.example", randSuffix())
	const password = "TestPass123!"

	// Register
	resp := env.do("POST", "/api/auth/register", "", map[string]any{
		"email": email, "password": password,
	})
	body := mustStatus(t, resp, http.StatusCreated)
	user := nested(t, body, "user")
	userID := str(t, user, "id")
	accessToken := str(t, body, "access_token")
	refreshToken := str(t, body, "refresh_token")

	return &testSession{
		env:          env,
		Email:        email,
		UserID:       userID,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}
}

// do is a convenience wrapper that injects the session token.
func (s *testSession) do(method, path string, body any) *http.Response {
	return s.env.do(method, path, s.AccessToken, body)
}

// ── Mock adapter ──────────────────────────────────────────────────────────────

// mockAdapter is a minimal adapter for integration testing.
// It does not use OAuth; credentials are accepted as opaque bytes.
type mockAdapter struct {
	serviceID string
	actions   []string
	result    *adapters.Result
	execErr   error
}

func newMockAdapter(serviceID string, actions ...string) *mockAdapter {
	return &mockAdapter{serviceID: serviceID, actions: actions}
}

func (m *mockAdapter) withResult(summary string, data any) *mockAdapter {
	m.result = &adapters.Result{Summary: summary, Data: data}
	return m
}

func (m *mockAdapter) withError(err error) *mockAdapter {
	m.execErr = err
	return m
}

func (m *mockAdapter) ServiceID() string          { return m.serviceID }
func (m *mockAdapter) SupportedActions() []string { return m.actions }

func (m *mockAdapter) Execute(_ context.Context, _ adapters.Request) (*adapters.Result, error) {
	return m.result, m.execErr
}

func (m *mockAdapter) OAuthConfig() *oauth2.Config { return nil }
func (m *mockAdapter) RequiredScopes() []string    { return nil }

func (m *mockAdapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error) {
	return []byte("mock-cred"), nil
}

func (m *mockAdapter) ValidateCredential(b []byte) error {
	if b == nil {
		return fmt.Errorf("mock: credential required")
	}
	return nil
}

// ── Scenario builder ──────────────────────────────────────────────────────────

// scenario groups the common objects needed for gateway tests:
// a user session and an agent token.
type scenario struct {
	session    *testSession
	AgentToken string
	AgentID    string
}

// newScenario creates a user and an agent. The roleName parameter is kept
// for call-site compatibility but is unused (roles have been removed).
func newScenario(t *testing.T, env *testEnv, _ string) *scenario {
	t.Helper()
	s := newSession(t, env)

	// Create agent
	resp := s.do("POST", "/api/agents", map[string]any{
		"name": "test-agent",
	})
	agentBody := mustStatus(t, resp, http.StatusCreated)
	agentToken := str(t, agentBody, "token")
	agentID := str(t, agentBody, "id")

	return &scenario{
		session:    s,
		AgentToken: agentToken,
		AgentID:    agentID,
	}
}

// activateService seeds a dummy vault credential so the service passes activation checks.
func (sc *scenario) activateService(t *testing.T, env *testEnv, service string) {
	t.Helper()
	cred := []byte(`{"type":"api_key","token":"test-token"}`)
	if err := env.Vault.Set(context.Background(), sc.session.UserID, service, cred); err != nil {
		t.Fatalf("activateService: vault.Set failed: %v", err)
	}
}

// createRestriction creates a restriction via the API and returns its ID.
func (sc *scenario) createRestriction(t *testing.T, service, action, reason string) string {
	t.Helper()
	resp := sc.session.do("POST", "/api/restrictions", map[string]any{
		"service": service, "action": action, "reason": reason,
	})
	body := mustStatus(t, resp, http.StatusCreated)
	return str(t, body, "id")
}

// createApprovedTask creates a task via agent token and approves it via user JWT.
// Returns the task ID.
func (sc *scenario) createApprovedTask(t *testing.T, env *testEnv, service, action string, autoExecute bool) string {
	t.Helper()
	sc.activateService(t, env, service)
	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "test task",
		"authorized_actions": []map[string]any{{
			"service": service, "action": action, "auto_execute": autoExecute,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	// Approve the task as user.
	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)
	return taskID
}

// createApprovedStandingTask creates a standing task and approves it. Returns the task ID.
func (sc *scenario) createApprovedStandingTask(t *testing.T, env *testEnv, service, action string, autoExecute bool) string {
	t.Helper()
	sc.activateService(t, env, service)
	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose":  "standing test task",
		"lifetime": "standing",
		"authorized_actions": []map[string]any{{
			"service": service, "action": action, "auto_execute": autoExecute,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)
	return taskID
}

// gatewayRequest sends a request through the gateway using the scenario's agent token.
func (sc *scenario) gatewayRequest(env *testEnv, reqID, service, action string) map[string]any {
	return sc.gatewayRequestWithTask(env, reqID, service, action, "")
}

// gatewayRequestWithTask sends a gateway request with an optional task_id.
func (sc *scenario) gatewayRequestWithTask(env *testEnv, reqID, service, action, taskID string) map[string]any {
	return sc.gatewayRequestWithTaskAndSession(env, reqID, service, action, taskID, "")
}

// gatewayRequestWithTaskAndSession sends a gateway request with an optional task_id and session_id.
func (sc *scenario) gatewayRequestWithTaskAndSession(env *testEnv, reqID, service, action, taskID, sessionID string) map[string]any {
	body := map[string]any{
		"service":    service,
		"action":     action,
		"params":     map[string]any{"to": "bob@example.com"},
		"reason":     "test reason",
		"request_id": reqID,
	}
	if taskID != "" {
		body["task_id"] = taskID
	}
	if sessionID != "" {
		body["session_id"] = sessionID
	}
	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, body)
	var m map[string]any
	decode(sc.session.env.t, resp, &m)
	return m
}

// ── Utility ───────────────────────────────────────────────────────────────────

var randCounter int

func randSuffix() string {
	randCounter++
	return fmt.Sprintf("%d", randCounter)
}

// strContains checks whether s contains substr, failing the test if not.
func strContains(t *testing.T, s, substr, msg string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("%s: expected %q to contain %q", msg, s, substr)
	}
}
