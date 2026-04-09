package smoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// e2eEnv holds a running clawvisor server process and auth tokens for tests.
type e2eEnv struct {
	t       *testing.T
	baseURL string
	tmpDir  string
	cmd     *exec.Cmd

	// Auth tokens obtained via magic link exchange.
	UserAccessToken  string
	UserRefreshToken string
	UserID           string

	// Agent credentials created during setup.
	AgentToken string
	AgentID    string

	client *http.Client
}

// configOverride is used to patch the copied config.yaml.
type configOverride struct {
	Server struct {
		Port int    `yaml:"port"`
		Host string `yaml:"host"`
	} `yaml:"server"`
	Database struct {
		Driver     string `yaml:"driver"`
		SQLitePath string `yaml:"sqlite_path"`
	} `yaml:"database"`
	Vault struct {
		Backend      string `yaml:"backend"`
		LocalKeyFile string `yaml:"local_key_file"`
	} `yaml:"vault"`
	Relay struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"relay"`
	Push struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"push"`
	Telemetry struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"telemetry"`
}

// newE2EEnv builds the clawvisor binary, copies the user's config/data to a
// temp directory, starts the server on a free port, and authenticates.
func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()

	if os.Getenv("CLAWVISOR_LOCAL_CONFIG") == "" {
		t.Skip("skipping: set CLAWVISOR_LOCAL_CONFIG=1 to run tests that use ~/.clawvisor/config.yaml")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	clawDir := filepath.Join(home, ".clawvisor")

	// Verify user config exists.
	srcConfig := filepath.Join(clawDir, "config.yaml")
	if _, err := os.Stat(srcConfig); err != nil {
		t.Skipf("skipping: ~/.clawvisor/config.yaml not found: %v", err)
	}

	// Resolve binary: use CLAWVISOR_BIN env, then ~/.clawvisor/bin/clawvisor,
	// then fall back to go build.
	binPath := os.Getenv("CLAWVISOR_BIN")
	if binPath == "" {
		installed := filepath.Join(clawDir, "bin", "clawvisor")
		if _, err := os.Stat(installed); err == nil {
			binPath = installed
		}
	}
	if binPath == "" {
		projectRoot, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatalf("project root: %v", err)
		}
		binPath = filepath.Join(projectRoot, "bin", "clawvisor-e2e")
		buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/clawvisor")
		buildCmd.Dir = projectRoot
		buildCmd.Stdout = os.Stderr
		buildCmd.Stderr = os.Stderr
		if err := buildCmd.Run(); err != nil {
			t.Fatalf("go build: %v", err)
		}
	}
	t.Logf("using binary: %s", binPath)

	// Create temp directory for this test run.
	tmpDir := t.TempDir()

	// Copy config.yaml and patch it.
	port := freePort(t)
	patchedConfig := patchConfig(t, srcConfig, tmpDir, port)
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, patchedConfig, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Copy vault key (needed to decrypt existing credentials in a copied DB).
	srcVaultKey := filepath.Join(clawDir, "vault.key")
	if _, err := os.Stat(srcVaultKey); err == nil {
		copyFile(t, srcVaultKey, filepath.Join(tmpDir, "vault.key"))
	}

	// Copy SQLite database so we have existing service credentials.
	srcDB := filepath.Join(clawDir, "clawvisor.db")
	if _, err := os.Stat(srcDB); err == nil {
		copyFile(t, srcDB, filepath.Join(tmpDir, "clawvisor.db"))
		// Also copy WAL/SHM if they exist.
		for _, ext := range []string{"-wal", "-shm"} {
			src := srcDB + ext
			if _, err := os.Stat(src); err == nil {
				copyFile(t, src, filepath.Join(tmpDir, "clawvisor.db"+ext))
			}
		}
	}

	// Start the server.
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, binPath, "server")
	cmd.Dir = tmpDir
	cmd.Env = append(os.Environ(),
		"CONFIG_FILE="+configPath,
		"HOME="+home,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start server: %v", err)
	}

	env := &e2eEnv{
		t:       t,
		baseURL: baseURL,
		tmpDir:  tmpDir,
		cmd:     cmd,
		client:  &http.Client{Timeout: 30 * time.Second},
	}

	// Store cancel in package-level var so TestMain can shut down the server.
	sharedCancel = cancel

	// Wait for the server to be ready.
	if !env.waitReady(20 * time.Second) {
		t.Fatalf("server did not become ready within 20s\nstderr:\n%s", stderr.String())
	}

	// Authenticate via magic link.
	env.authenticate(t)

	// Create an agent for gateway tests.
	env.createAgent(t)

	return env
}

// patchConfig reads the source config, overrides port/db/vault paths to use
// the temp dir, and disables relay/push/telemetry.
func patchConfig(t *testing.T, srcPath, tmpDir string, port int) []byte {
	t.Helper()

	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	// Parse into a generic map so we preserve all fields.
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}

	// Patch server.
	server, _ := cfg["server"].(map[string]any)
	if server == nil {
		server = map[string]any{}
	}
	server["port"] = port
	server["host"] = "127.0.0.1"
	cfg["server"] = server

	// Patch database.
	database, _ := cfg["database"].(map[string]any)
	if database == nil {
		database = map[string]any{}
	}
	database["driver"] = "sqlite"
	database["sqlite_path"] = filepath.Join(tmpDir, "clawvisor.db")
	cfg["database"] = database

	// Patch vault.
	vault, _ := cfg["vault"].(map[string]any)
	if vault == nil {
		vault = map[string]any{}
	}
	vault["backend"] = "local"
	vault["local_key_file"] = filepath.Join(tmpDir, "vault.key")
	cfg["vault"] = vault

	// Disable relay, push, telemetry.
	relay, _ := cfg["relay"].(map[string]any)
	if relay == nil {
		relay = map[string]any{}
	}
	relay["enabled"] = false
	cfg["relay"] = relay

	push, _ := cfg["push"].(map[string]any)
	if push == nil {
		push = map[string]any{}
	}
	push["enabled"] = false
	cfg["push"] = push

	cfg["telemetry"] = map[string]any{"enabled": false}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return out
}

// freePort finds an available TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// copyFile copies src to dst.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// waitReady polls /health until it returns 200 or the timeout expires.
func (e *e2eEnv) waitReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := e.client.Get(e.baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// authenticate gets a magic token via /api/auth/magic/local, then exchanges
// it for JWT tokens via /api/auth/magic.
func (e *e2eEnv) authenticate(t *testing.T) {
	t.Helper()

	// Step 1: Get a magic token from the local endpoint.
	resp := e.doRaw("POST", "/api/auth/magic/local", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("magic/local: status %d: %s", resp.StatusCode, body)
	}
	var magicResp struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&magicResp); err != nil {
		t.Fatalf("decode magic/local: %v", err)
	}

	// Step 2: Exchange magic token for JWT.
	resp2 := e.doRaw("POST", "/api/auth/magic", "", map[string]any{
		"token": magicResp.Token,
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("magic exchange: status %d: %s", resp2.StatusCode, body)
	}
	var authResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		User         struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&authResp); err != nil {
		t.Fatalf("decode auth: %v", err)
	}

	e.UserAccessToken = authResp.AccessToken
	e.UserRefreshToken = authResp.RefreshToken
	e.UserID = authResp.User.ID
	t.Logf("authenticated as %s (id=%s)", authResp.User.Email, e.UserID)
}

// createAgent creates a test agent and stores the token.
func (e *e2eEnv) createAgent(t *testing.T) {
	t.Helper()
	resp := e.userDo("POST", "/api/agents", map[string]any{
		"name": "e2e-smoke-test",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create agent: status %d: %s", resp.StatusCode, body)
	}
	var agentResp map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&agentResp); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	e.AgentToken = agentResp["token"].(string)
	e.AgentID = agentResp["id"].(string)
	t.Logf("created agent %s", e.AgentID)
}

// userDo makes an HTTP request with the user's JWT.
func (e *e2eEnv) userDo(method, path string, body any) *http.Response {
	return e.doRaw(method, path, e.UserAccessToken, body)
}

// agentDo makes an HTTP request with the agent bearer token.
func (e *e2eEnv) agentDo(method, path string, body any) *http.Response {
	return e.doRaw(method, path, e.AgentToken, body)
}

// doRaw performs an HTTP request, JSON-encoding body if non-nil.
func (e *e2eEnv) doRaw(method, path string, token string, body any) *http.Response {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal: %v", err)
		}
		r = bytes.NewReader(b)
	}
	url := e.baseURL + path
	req, err := http.NewRequest(method, url, r)
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

// decodeJSON reads and decodes a JSON response body.
func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode (status=%d body=%s): %v", resp.StatusCode, b, err)
	}
}

// mustStatus asserts the response has the expected status and returns the
// decoded body as a map.
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
		t.Fatalf("decode (status=%d body=%s): %v", resp.StatusCode, b, err)
	}
	return m
}

// skipIfServiceNotActivated checks whether a service has credentials in the
// vault by looking at the services list. Skips the test if not.
func (e *e2eEnv) skipIfServiceNotActivated(t *testing.T, serviceID string) {
	t.Helper()
	resp := e.userDo("GET", "/api/services", nil)
	m := mustStatus(t, resp, http.StatusOK)

	list, ok := m["services"].([]any)
	if !ok {
		t.Skipf("skipping: could not parse services list")
	}
	for _, raw := range list {
		svc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := strOr(svc, "id", "")
		if id == serviceID {
			status := strOr(svc, "status", "")
			if status == "activated" || status == "active" || status == "connected" {
				return
			}
		}
	}
	t.Skipf("skipping: service %q not activated", serviceID)
}

// str extracts a string from a decoded map.
func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q not found in %v", key, m)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("key %q is not a string (got %T)", key, v)
	}
	return s
}

// strOr extracts a string from a map, returning a default if missing.
func strOr(m map[string]any, key, fallback string) string {
	v, ok := m[key]
	if !ok {
		return fallback
	}
	s, ok := v.(string)
	if !ok {
		return fallback
	}
	return s
}

// containsStr returns true if s contains substr.
func containsStr(s, substr string) bool {
	return strings.Contains(s, substr)
}

// readServiceCandidates maps service IDs to read-only actions with default params.
// Used by pickActivatedReadService to find a testable service dynamically.
var readServiceCandidates = []struct {
	svcID  string
	action string
	params map[string]any
}{
	{"linear", "list_issues", map[string]any{}},
	{"github", "list_repos", map[string]any{}},
	{"google.gmail", "list_messages", map[string]any{}},
	{"google.calendar", "list_events", map[string]any{}},
	{"google.drive", "list_files", map[string]any{}},
	{"google.contacts", "list_contacts", map[string]any{}},
	{"slack", "list_channels", map[string]any{}},
	{"notion", "list_databases", map[string]any{}},
}

// secondReadActions maps service IDs to their secondary read actions (for scope expansion tests).
var secondReadActions = map[string]string{
	"linear":          "list_teams",
	"github":          "list_issues",
	"google.gmail":    "get_message",
	"google.calendar": "list_calendars",
	"google.drive":    "search_files",
	"slack":           "list_users",
}

// pickActivatedReadService returns the first activated service with a read-only
// action from the candidates list. Skips the test if none are activated.
func (e *e2eEnv) pickActivatedReadService(t *testing.T) (svcID, action string, params map[string]any) {
	t.Helper()

	resp := e.userDo("GET", "/api/services", nil)
	m := mustStatus(t, resp, http.StatusOK)
	list, ok := m["services"].([]any)
	if !ok {
		t.Skipf("skipping: could not parse services list")
	}

	// activated maps base service ID → full service ID (with alias if present).
	activated := map[string]string{}
	for _, raw := range list {
		svc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := strOr(svc, "id", "")
		alias := strOr(svc, "alias", "")
		status := strOr(svc, "status", "")
		if status == "activated" || status == "active" || status == "connected" {
			fullID := id
			if alias != "" {
				fullID = id + ":" + alias
			}
			activated[id] = fullID
		}
	}

	for _, c := range readServiceCandidates {
		if fullID, ok := activated[c.svcID]; ok {
			return fullID, c.action, c.params
		}
	}

	t.Skipf("skipping: no activated read-only service found (activated: %v)", activated)
	return "", "", nil
}

// pickSecondAction returns a second read action for scope expansion tests.
// Returns "" if no second action is available for the service.
func (e *e2eEnv) pickSecondAction(t *testing.T, svcID, firstAction string) string {
	t.Helper()
	// Strip alias (e.g. "github:ericlevine" → "github") for lookup.
	baseID := svcID
	if idx := strings.IndexByte(svcID, ':'); idx >= 0 {
		baseID = svcID[:idx]
	}
	second, ok := secondReadActions[baseID]
	if !ok || second == firstAction {
		return ""
	}
	return second
}

// waitAndApproveTask polls a task until it's ready for approval, then approves it.
// Works with any requester function (agent token from harness or from connection flow).
func (e *e2eEnv) waitAndApproveTask(t *testing.T, agentReq func(string, string, any) *http.Response, taskID string) {
	t.Helper()

	for i := 0; i < 15; i++ {
		checkResp := agentReq("GET", fmt.Sprintf("/api/tasks/%s", taskID), nil)
		checkM := mustStatus(t, checkResp, http.StatusOK)
		s := strOr(checkM, "status", "")
		if s == "pending_approval" || s == "pending" {
			break
		}
		if s == "approved" || s == "active" {
			return // already approved
		}
		time.Sleep(300 * time.Millisecond)
	}

	approveResp := e.userDo("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	defer approveResp.Body.Close()
	approveBody, _ := io.ReadAll(approveResp.Body)
	if approveResp.StatusCode != http.StatusOK {
		// Check if auto-approved during the loop.
		checkResp := agentReq("GET", fmt.Sprintf("/api/tasks/%s", taskID), nil)
		checkM := mustStatus(t, checkResp, http.StatusOK)
		s := strOr(checkM, "status", "")
		if s != "approved" && s != "active" {
			t.Fatalf("approve task %s: status %d: %s (task status: %s)", taskID, approveResp.StatusCode, approveBody, s)
		}
	}
}
