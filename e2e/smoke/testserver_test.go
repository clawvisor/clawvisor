package smoke_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// ── Mock API Server ──────────────────────────────────────────────────────────

// startMockAPIServer returns an httptest.Server that acts as both an OAuth
// provider (authorize/token endpoints) and the API backend for all three
// test adapter services: test_oauth, test_apikey, test_noauth.
func startMockAPIServer() *httptest.Server {
	mux := http.NewServeMux()

	// OAuth token endpoint — exchanges an authorization code for tokens.
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		code := r.FormValue("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "mock-oauth-access-token",
			"token_type":    "Bearer",
			"refresh_token": "mock-oauth-refresh-token",
			"expires_in":    3600,
		})
	})

	// test_oauth API — requires Bearer token.
	mux.HandleFunc("/test_oauth/items", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]string{
				{"id": "1", "name": "alpha"},
				{"id": "2", "name": "beta"},
			},
		})
	})

	// test_apikey API — requires X-API-Key header.
	mux.HandleFunc("/test_apikey/items", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]string{
				{"id": "1", "name": "gamma"},
				{"id": "2", "name": "delta"},
			},
		})
	})

	// test_noauth API — no auth required.
	mux.HandleFunc("/test_noauth/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]string{
				{"id": "1", "name": "epsilon"},
				{"id": "2", "name": "zeta"},
			},
		})
	})

	srv := httptest.NewServer(mux)
	return srv
}

// ── Test Adapter YAML Definitions ────────────────────────────────────────────

func writeTestAdapterYAMLs(t *testing.T, adaptersDir, mockURL string) {
	t.Helper()

	adapters := map[string]string{
		"test_oauth.yaml":  testOAuthYAML(mockURL),
		"test_apikey.yaml": testAPIKeyYAML(mockURL),
		"test_noauth.yaml": testNoAuthYAML(mockURL),
	}
	for name, content := range adapters {
		path := filepath.Join(adaptersDir, name)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		t.Logf("wrote adapter: %s (%d bytes)", path, len(content))
	}
}

func testOAuthYAML(mockURL string) string {
	return fmt.Sprintf(`service:
  id: test_oauth
  display_name: Test OAuth Service
  description: Test service with OAuth2 authentication

auth:
  type: oauth2
  header: Authorization
  header_prefix: "Bearer "
  oauth:
    client_id: "test-client-id"
    client_secret: "test-client-secret"
    authorize_url: "%s/oauth/authorize"
    token_url: "%s/oauth/token"
    scopes:
      - "read"

api:
  base_url: "%s/test_oauth"
  type: rest

actions:
  list_items:
    display_name: List Items
    risk:
      category: read
      sensitivity: low
      description: List test items
    method: GET
    path: /items
    response:
      summary: "Listed test items"
  create_item:
    display_name: Create Item
    risk:
      category: write
      sensitivity: medium
      description: Create a test item
    method: POST
    path: /items
    params:
      name:
        type: string
        required: true
        location: body
    response:
      summary: "Created test item"
`, mockURL, mockURL, mockURL)
}

func testAPIKeyYAML(mockURL string) string {
	return fmt.Sprintf(`service:
  id: test_apikey
  display_name: Test API Key Service
  description: Test service with API key authentication

auth:
  type: api_key
  header: X-API-Key

api:
  base_url: "%s/test_apikey"
  type: rest

actions:
  list_items:
    display_name: List Items
    risk:
      category: read
      sensitivity: low
      description: List test items
    method: GET
    path: /items
    response:
      summary: "Listed test items"
  create_item:
    display_name: Create Item
    risk:
      category: write
      sensitivity: medium
      description: Create a test item
    method: POST
    path: /items
    params:
      name:
        type: string
        required: true
        location: body
    response:
      summary: "Created test item"
`, mockURL)
}

func testNoAuthYAML(mockURL string) string {
	return fmt.Sprintf(`service:
  id: test_noauth
  display_name: Test No-Auth Service
  description: Test service requiring no credentials

auth:
  type: none

api:
  base_url: "%s/test_noauth"
  type: rest

actions:
  list_items:
    display_name: List Items
    risk:
      category: read
      sensitivity: low
      description: List test items
    method: GET
    path: /items
    response:
      summary: "Listed test items"
  create_item:
    display_name: Create Item
    risk:
      category: write
      sensitivity: medium
      description: Create a test item
    method: POST
    path: /items
    params:
      name:
        type: string
        required: true
        location: body
    response:
      summary: "Created test item"
`, mockURL)
}

// ── CI Environment ───────────────────────────────────────────────────────────

// Shared CI environment — started once per test run.
var ciSharedEnv *ciTestEnv

// ciTestEnv wraps e2eEnv with mock server details.
type ciTestEnv struct {
	*e2eEnv
	mockURL string
	mockSrv *httptest.Server
	cancel  context.CancelFunc
}

// ciSetup returns a shared CI test environment. The first call starts the mock
// API server, generates a fresh config/DB/vault, writes test adapter YAMLs,
// and starts the clawvisor server. Subsequent calls return the cached env.
func ciSetup(t *testing.T) *ciTestEnv {
	t.Helper()
	if ciSharedEnv == nil {
		ciSharedEnv = newCIEnv(t)
	}
	return ciSharedEnv
}

func newCIEnv(t *testing.T) *ciTestEnv {
	t.Helper()

	// Start mock API server.
	mockSrv := startMockAPIServer()
	mockURL := mockSrv.URL
	t.Logf("mock API server: %s", mockURL)

	// Create temp directory tree.
	tmpDir := t.TempDir()
	clawDir := filepath.Join(tmpDir, ".clawvisor")
	adaptersDir := filepath.Join(clawDir, "adapters")
	if err := os.MkdirAll(adaptersDir, 0755); err != nil {
		t.Fatalf("mkdir adapters: %v", err)
	}

	// Write test adapter YAMLs.
	writeTestAdapterYAMLs(t, adaptersDir, mockURL)

	// Generate vault key (32 random bytes, base64-encoded).
	vaultKey := make([]byte, 32)
	if _, err := rand.Read(vaultKey); err != nil {
		t.Fatalf("generate vault key: %v", err)
	}
	vaultKeyPath := filepath.Join(clawDir, "vault.key")
	if err := os.WriteFile(vaultKeyPath, []byte(base64.StdEncoding.EncodeToString(vaultKey)), 0600); err != nil {
		t.Fatalf("write vault key: %v", err)
	}

	// Generate minimal config.
	port := freePort(t)
	dbPath := filepath.Join(clawDir, "clawvisor.db")
	cfg := map[string]any{
		"server":    map[string]any{"port": port, "host": "127.0.0.1"},
		"database":  map[string]any{"driver": "sqlite", "sqlite_path": dbPath},
		"vault":     map[string]any{"backend": "local", "local_key_file": vaultKeyPath},
		"relay":     map[string]any{"enabled": false},
		"push":      map[string]any{"enabled": false},
		"telemetry": map[string]any{"enabled": false},
	}
	cfgBytes, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configPath := filepath.Join(clawDir, "config.yaml")
	if err := os.WriteFile(configPath, cfgBytes, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Logf("CI config: port=%d, db=%s", port, dbPath)

	// Build binary.
	binPath := resolveOrBuildBinary(t)

	// Start the server with HOME=tmpDir so it finds adapters in tmpDir/.clawvisor/adapters/.
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, binPath, "server")
	cmd.Dir = clawDir
	cmd.Env = []string{
		"HOME=" + tmpDir,
		"CONFIG_FILE=" + configPath,
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.Getenv("TMPDIR"),
	}
	// Inherit CGO-related vars for SQLite.
	for _, key := range []string{"CGO_ENABLED", "CC", "CXX"} {
		if v := os.Getenv(key); v != "" {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start CI server: %v", err)
	}
	t.Logf("CI server started (pid=%d)", cmd.Process.Pid)

	env := &e2eEnv{
		t:       t,
		baseURL: baseURL,
		tmpDir:  tmpDir,
		cmd:     cmd,
		client:  &http.Client{Timeout: 30 * time.Second},
	}

	if !env.waitReady(20 * time.Second) {
		cancel()
		t.Fatal("CI server did not become ready within 20s")
	}
	t.Log("CI server ready")

	// Authenticate via magic link.
	env.authenticate(t)

	// Create an agent for tests.
	env.createAgent(t)

	return &ciTestEnv{
		e2eEnv:  env,
		mockURL: mockURL,
		mockSrv: mockSrv,
		cancel:  cancel,
	}
}

// resolveOrBuildBinary returns the explicitly requested test binary or builds
// the current workspace's CLI binary so smoke tests always exercise the code
// under test instead of a potentially stale installed copy.
func resolveOrBuildBinary(t *testing.T) string {
	t.Helper()

	if binPath := os.Getenv("CLAWVISOR_BIN"); binPath != "" {
		return binPath
	}

	projectRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("project root: %v", err)
	}
	binPath := filepath.Join(projectRoot, "bin", "clawvisor-e2e")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/clawvisor")
	buildCmd.Dir = projectRoot
	buildCmd.Stdout = os.Stderr
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}
	return binPath
}

// ── Service Activation Helpers ───────────────────────────────────────────────

// activateTestOAuth performs the full OAuth dance:
// 1. POST /api/services/test_oauth/activate → get auth URL with state
// 2. Call /api/oauth/callback with the state and a test code
// 3. Server exchanges code at mock token endpoint → stores credential
func (e *ciTestEnv) activateTestOAuth(t *testing.T) {
	t.Helper()

	// Step 1: Start OAuth flow.
	resp := e.userDo("POST", "/api/services/test_oauth/activate", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body := readBody(resp)
		t.Fatalf("activate test_oauth: status %d: %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode activate response: %v", err)
	}

	// If already authorized, we're done.
	if already, _ := result["already_authorized"].(bool); already {
		t.Log("test_oauth already authorized")
		return
	}

	authURL, _ := result["url"].(string)
	if authURL == "" {
		t.Fatalf("activate returned no url: %v", result)
	}
	t.Logf("OAuth auth URL: %s", authURL)

	// Step 2: Extract state from the auth URL.
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatal("auth URL missing state parameter")
	}

	// Step 3: Call the callback directly (simulates browser redirect).
	callbackPath := fmt.Sprintf("/api/oauth/callback?code=test-auth-code&state=%s", url.QueryEscape(state))
	callbackResp := e.doRaw("GET", callbackPath, "", nil)
	defer callbackResp.Body.Close()
	// Callback returns HTML, not JSON. We just need it to succeed (200).
	if callbackResp.StatusCode != http.StatusOK {
		body := readBody(callbackResp)
		t.Fatalf("OAuth callback: status %d: %s", callbackResp.StatusCode, body)
	}
	t.Log("OAuth callback completed")

	// Step 4: Verify service is activated.
	e.verifyServiceActivated(t, "test_oauth")
}

// activateTestAPIKey activates the test_apikey service with a test key.
func (e *ciTestEnv) activateTestAPIKey(t *testing.T) {
	t.Helper()

	resp := e.userDo("POST", "/api/services/test_apikey/activate", map[string]any{
		"token": "test-api-key-12345",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body := readBody(resp)
		t.Fatalf("activate test_apikey: status %d: %s", resp.StatusCode, body)
	}
	t.Log("test_apikey activated")

	e.verifyServiceActivated(t, "test_apikey")
}

// activateTestNoAuth activates the credential-free test_noauth service.
func (e *ciTestEnv) activateTestNoAuth(t *testing.T) {
	t.Helper()

	resp := e.userDo("POST", "/api/services/test_noauth/activate", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body := readBody(resp)
		t.Fatalf("activate test_noauth: status %d: %s", resp.StatusCode, body)
	}
	t.Log("test_noauth activated")

	e.verifyServiceActivated(t, "test_noauth")
}

// verifyServiceActivated checks that a service shows as activated in the services list.
func (e *ciTestEnv) verifyServiceActivated(t *testing.T, serviceID string) {
	t.Helper()
	resp := e.userDo("GET", "/api/services", nil)
	m := mustStatus(t, resp, http.StatusOK)

	list, ok := m["services"].([]any)
	if !ok {
		t.Fatalf("could not parse services list")
	}
	for _, raw := range list {
		svc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strOr(svc, "id", "") == serviceID {
			status := strOr(svc, "status", "")
			if status == "activated" || status == "active" || status == "connected" {
				t.Logf("verified: %s is %s", serviceID, status)
				return
			}
			t.Fatalf("service %s has status %q (expected activated)", serviceID, status)
		}
	}
	t.Fatalf("service %s not found in services list", serviceID)
}

// readBody is a small helper that drains a response body to a string.
func readBody(resp *http.Response) string {
	b := make([]byte, 4096)
	n, _ := resp.Body.Read(b)
	return string(b[:n])
}
