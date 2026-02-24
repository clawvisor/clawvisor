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

	"github.com/ericlevine/clawvisor/internal/adapters"
	"github.com/ericlevine/clawvisor/internal/api"
	"github.com/ericlevine/clawvisor/internal/auth"
	"github.com/ericlevine/clawvisor/internal/config"
	"github.com/ericlevine/clawvisor/internal/policy"
	"github.com/ericlevine/clawvisor/internal/safety"
	"github.com/ericlevine/clawvisor/internal/store"
	sqlitestore "github.com/ericlevine/clawvisor/internal/store/sqlite"
	"github.com/ericlevine/clawvisor/internal/vault"
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
	t.Helper()

	ctx := context.Background()

	// SQLite in a temp directory (t.Cleanup will remove it)
	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	st := sqlitestore.NewStore(db)

	// LocalVault with auto-generated key
	v, err := vault.NewLocalVault(t.TempDir()+"/vault.key", db, "sqlite")
	if err != nil {
		t.Fatalf("vault: %v", err)
	}

	jwtSvc, err := auth.NewJWTService("test-secret-for-integration-tests")
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}

	reg := policy.NewRegistry()

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
	}

	srv, err := api.New(cfg, st, v, jwtSvc, reg, adapterReg, nil, safety.NoopChecker{}, config.LLMConfig{})
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

func (m *mockAdapter) ServiceID() string       { return m.serviceID }
func (m *mockAdapter) SupportedActions() []string { return m.actions }

func (m *mockAdapter) Execute(_ context.Context, _ adapters.Request) (*adapters.Result, error) {
	return m.result, m.execErr
}

func (m *mockAdapter) OAuthConfig() *oauth2.Config { return nil }

func (m *mockAdapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error) {
	return []byte("mock-cred"), nil
}

func (m *mockAdapter) ValidateCredential(b []byte) error {
	if b == nil {
		return fmt.Errorf("mock: credential required")
	}
	return nil
}

// ── Policy YAML helpers ───────────────────────────────────────────────────────

// blockPolicy returns a YAML policy string that blocks the given service+action for roleName.
func blockPolicy(id, roleName, service, action string) string {
	return fmt.Sprintf(`id: %s
name: Block %s %s
role: %s
rules:
  - service: %s
    actions: [%s]
    allow: false
    reason: Blocked by test policy
`, id, service, action, roleName, service, action)
}

// approvePolicy returns a YAML policy string that requires approval.
func approvePolicy(id, roleName, service, action string) string {
	return fmt.Sprintf(`id: %s
name: Approve %s/%s
role: %s
rules:
  - service: %s
    actions: [%s]
    require_approval: true
    reason: Requires human approval
`, id, service, action, roleName, service, action)
}

// executePolicy returns a YAML policy string that allows execution.
func executePolicy(id, roleName, service, action string) string {
	return fmt.Sprintf(`id: %s
name: Execute %s/%s
role: %s
rules:
  - service: %s
    actions: [%s]
    allow: true
`, id, service, action, roleName, service, action)
}

// ── Scenario builder ──────────────────────────────────────────────────────────

// scenario groups the common objects needed for gateway tests:
// a user session, a role, and an agent token.
type scenario struct {
	session    *testSession
	RoleID     string
	RoleName   string
	AgentToken string
	AgentID    string
}

// newScenario creates a user, a role named roleName, and an agent with that role.
func newScenario(t *testing.T, env *testEnv, roleName string) *scenario {
	t.Helper()
	s := newSession(t, env)

	// Create role
	resp := s.do("POST", "/api/roles", map[string]any{
		"name": roleName, "description": "test role",
	})
	roleBody := mustStatus(t, resp, http.StatusCreated)
	roleID := str(t, roleBody, "id")

	// Create agent
	resp = s.do("POST", "/api/agents", map[string]any{
		"name": "test-agent", "role_id": roleID,
	})
	agentBody := mustStatus(t, resp, http.StatusCreated)
	agentToken := str(t, agentBody, "token")
	agentID := str(t, agentBody, "id")

	return &scenario{
		session:    s,
		RoleID:     roleID,
		RoleName:   roleName,
		AgentToken: agentToken,
		AgentID:    agentID,
	}
}

// createPolicy creates a policy via the API and returns its ID.
func (sc *scenario) createPolicy(t *testing.T, yamlStr string) string {
	t.Helper()
	resp := sc.session.do("POST", "/api/policies", map[string]any{"yaml": yamlStr})
	body := mustStatus(t, resp, http.StatusCreated)
	return str(t, body, "id")
}

// gatewayRequest sends a request through the gateway using the scenario's agent token.
func (sc *scenario) gatewayRequest(env *testEnv, reqID, service, action string) map[string]any {
	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    service,
		"action":     action,
		"params":     map[string]any{"to": "bob@example.com"},
		"reason":     "test reason",
		"request_id": reqID,
	})
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
