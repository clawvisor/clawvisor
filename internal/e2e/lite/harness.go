package lite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/handlers"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/scriptjudge/llmjudge"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

// Env vars the harness reads for upstream API keys. Test skips when none
// are set.
const (
	EnvAnthropicKey       = "CLAWVISOR_LLM_API_KEY"
	EnvAnthropicKeyLegacy = "CLAWVISOR_E2E_ANTHROPIC_KEY"
	EnvOpenAIKey          = "CLAWVISOR_OPENAI_KEY"
)

// ResolveAnthropicKey returns the Anthropic API key from env or "".
func ResolveAnthropicKey() string {
	if v := os.Getenv(EnvAnthropicKey); v != "" {
		return v
	}
	return os.Getenv(EnvAnthropicKeyLegacy)
}

// ResolveOpenAIKey returns the OpenAI API key from env or "".
func ResolveOpenAIKey() string {
	return os.Getenv(EnvOpenAIKey)
}

// Keys bundles the upstream provider keys to plant in the user's vault.
// Each is optional — drivers that need a key for which we have none will
// be skipped at runtime.
type Keys struct {
	Anthropic string
	OpenAI    string
}

// Harness is one scenario run's wired stack: real sqlite store, real
// adapter registry, real lite-proxy LLMEndpointHandler with a real
// TasksHandler wired as InlineTaskCreator. The agent driver (claude or
// codex) talks to Endpoint as if it were Anthropic / OpenAI.
type Harness struct {
	Store      store.Store
	Endpoint   *httptest.Server
	UserID     string
	AgentID    string
	AgentToken string
	Workspace  string
	Counters   *Counters
	Logger     *slog.Logger

	// MockUpstream is an httptest server scenarios direct the agent to
	// for a "downstream" call after a credentialed task has been
	// approved. Every incoming request increments
	// downstream.calls_total, and requests whose headers contain one
	// of the minted credential placeholders increment
	// downstream.placeholder_used.
	MockUpstream *httptest.Server

	// MCPConfigPath is the absolute path to a workspace-local .mcp.json
	// written when scn.MCPStub is true. Empty otherwise. lite_test.go
	// passes this through to drivers.Config so Claude Code can be
	// pointed at the stub MCP server via --mcp-config.
	MCPConfigPath string

	// recorder is the InlineTaskCreator wrapper used by the mock
	// upstream to recognize minted placeholders. Exposed so test
	// helpers can also inspect what was minted.
	recorder *recordingInlineCreator
}

// MCPStubMarkerFilename is the workspace-relative path the stub MCP
// server writes when its `authenticate` tool is invoked. Exposed so
// scenarios can name it in `files_absent` to assert the agent did NOT
// reach for harness-side auth.
const MCPStubMarkerFilename = ".mcp_auth_marker"

// Start boots the harness for one scenario × driver run. The workspace
// is library/<scenario>/workspace copied to t.TempDir() (then
// setup_shell, if any).
func Start(t *testing.T, scn *Scenario, keys Keys) (*Harness, error) {
	t.Helper()
	if keys.Anthropic == "" && keys.OpenAI == "" {
		return nil, errors.New("at least one upstream API key (Anthropic or OpenAI) must be set")
	}
	ctx := context.Background()

	dataDir := t.TempDir()
	db, err := sqlite.New(ctx, filepath.Join(dataDir, "lite.db"))
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "lite-e2e@example.com", "x")
	if err != nil {
		return nil, err
	}
	rawToken, err := auth.GenerateAgentToken()
	if err != nil {
		return nil, err
	}
	agent, err := st.CreateAgent(ctx, user.ID, scn.Agent.Name, auth.HashToken(rawToken))
	if err != nil {
		return nil, err
	}

	v := newMemoryVault()
	if keys.Anthropic != "" {
		if err := v.Set(ctx, user.ID, "anthropic", []byte(keys.Anthropic)); err != nil {
			return nil, err
		}
	}
	if keys.OpenAI != "" {
		if err := v.Set(ctx, user.ID, "openai", []byte(keys.OpenAI)); err != nil {
			return nil, err
		}
	}
	plantedVaultIDs := make([]string, 0, len(scn.VaultItems))
	plantedSecrets := make([]string, 0, len(scn.VaultItems))
	for _, item := range scn.VaultItems {
		id := item.ID
		if id == "" {
			return nil, errors.New("scenario vault_items entry missing id")
		}
		if err := v.Set(ctx, user.ID, id, []byte(item.Secret)); err != nil {
			return nil, err
		}
		plantedVaultIDs = append(plantedVaultIDs, id)
		if item.Secret != "" {
			plantedSecrets = append(plantedSecrets, item.Secret)
		}
	}

	if err := seedPreseededTasks(ctx, st, user.ID, agent.ID, scn.PreseededTasks); err != nil {
		return nil, err
	}

	logger := slog.New(slog.NewTextHandler(testLogWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cfgPtr := config.Default()
	cfg := *cfgPtr
	// Empty adapter registry — scenarios in this harness exercise local
	// tools (Bash/Read/Write/Edit), not credentialed gateway adapters.
	reg := adapters.NewRegistry()
	hub := events.NewHub()
	assessor := taskrisk.NoopAssessor{}

	tasksHandler := handlers.NewTasksHandler(st, v, reg, nil /*notify*/, cfg, logger, "" /*baseURL*/, hub, assessor)

	counters := NewCounters()
	recorder := newRecordingInlineCreator(tasksHandler, counters, plantedVaultIDs)

	h := handlers.NewLLMEndpointHandler(st, v, logger)
	h.Forwarder = llmproxy.NewForwarder(v)
	h.Inspector = inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	h.InlineTaskCreator = recorder
	h.TaskScope = llmproxy.NewStoreTaskScopeChecker(st)

	// Use-time script-session judge: when the agent's tool_use carries
	// cv-script + autovault signals but the literal-prefix recognizer
	// can't see the URL (variable-ization, Write+Bash staging, language
	// wrappers), the judge re-classifies via an LLM and either allows
	// passthrough (resolver still enforces scope) or returns specific
	// agent-actionable guidance.
	//
	// The judge runs at the proxy layer independent of which driver
	// the test is exercising, so we wire it from whichever LLM key
	// is available — preferring Anthropic (Haiku is the canonical
	// judge model) and falling back to OpenAI when only that key is
	// set. Without this fallback, tests running with only an OpenAI
	// key would silently bypass every judge code path the
	// script_session_inline_fanout / script_session_long_fanout_no_staging
	// scenarios are meant to exercise.
	if judgeCfg, ok := buildJudgeConfig(keys); ok {
		h.ScriptSessionJudge = llmjudge.New(
			func() config.VerificationConfig { return judgeCfg },
			logger,
		)
	}

	// Script sessions: the agent can mint one via the control
	// endpoint and use it as caller-auth on later resolver calls. We
	// instrument the cache + mint handler so scenarios can assert on
	// SeriesScriptSessionMint / SeriesScriptSessionUse counters.
	scriptSessions := &countingScriptSessionCache{
		inner:    llmproxy.NewMemoryScriptSessionCache(),
		counters: counters,
	}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))
	mux.Handle("POST /v1/messages/count_tokens", mw(http.HandlerFunc(h.Messages)))
	mux.Handle("POST /v1/chat/completions", mw(http.HandlerFunc(h.ChatCompletions)))
	mux.Handle("POST /v1/responses", mw(http.HandlerFunc(h.Responses)))
	// /v1/models — the codex CLI polls this periodically to refresh
	// its model list. With no handler it 404s, and the CLI's
	// codex_models_manager retries indefinitely; under load it can
	// outrun the actual scenario work and trip the codex run's
	// context deadline, killing the subprocess mid-scenario. A
	// minimal openai-shaped stub keeps the manager happy.
	mux.Handle("GET /v1/models", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5-codex","object":"model","owned_by":"clawvisor-test"}]}`))
	}))

	// /api/control/vault/items: the only control-plane GET the lite
	// harness needs the agent to be able to discover available vault
	// items unaided. The proxy's control rewriter (postprocess.go)
	// converts `https://clawvisor.local/control/vault/items` in a
	// bash tool_use into `<ControlBaseURL>/api/control/vault/items`
	// with a freshly-minted nonce in X-Clawvisor-Caller. The mount
	// below shares h.CallerNonces so the nonce consumed here is the
	// same one the rewriter minted.
	vaultHandler := handlers.NewVaultHandler(st, v, reg)
	nonceMW := middleware.RequireAgentLLMNonce(st, h.CallerNonces, scriptSessions, logger)
	listVaultItems := nonceMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counters.Inc(SeriesVaultItemsListed)
		vaultHandler.ListForAgent(w, r)
	}))
	mux.Handle("GET /api/control/vault/items", listVaultItems)

	// Mint endpoint for autovault script sessions. Constructed
	// inline rather than via the production wiring so the harness
	// stays self-contained — no intent verifier (scenarios are
	// behavior-driven; verifier round-trips would add cost and
	// flakiness for tests that should just exercise the mechanism).
	controlHandler := handlers.NewLLMControlHandler("")
	controlHandler.Store = st
	controlHandler.ScriptSessions = scriptSessions
	mux.Handle("POST /api/control/autovault/script-session", nonceMW(http.HandlerFunc(controlHandler.MintScriptSession)))
	mux.Handle("GET /api/control/autovault/script", http.HandlerFunc(controlHandler.AutovaultScriptDocs))

	// /api/control/tasks: lets the agent discover already-approved
	// tasks (especially standing tasks carried over from prior
	// conversations) before POSTing a fresh one. The counter proves
	// the agent reached for discovery instead of blindly creating.
	listTasks := nonceMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counters.Inc(SeriesTasksListed)
		controlHandler.ListTasks(w, r)
	}))
	mux.Handle("GET /api/control/tasks", listTasks)

	// Mock upstream stands up before the resolver mount so the
	// resolver's custom DialContext can target it directly.
	mockUpstream := newMockUpstream(counters, recorder.PlaceholderSnapshot, plantedSecrets)
	t.Cleanup(mockUpstream.Close)
	mockAddr, err := mockUpstreamDialAddr(mockUpstream.URL)
	if err != nil {
		return nil, err
	}

	// /api/proxy/ — the credentialed-resolver path. The proxy's
	// rewriter intercepts the agent's outbound credentialed curl and
	// redirects it to `<ResolverBaseURL>/<upstream-path>` with the
	// upstream host moved to X-Clawvisor-Target-Host. A thin in-process
	// resolver below swaps the placeholder for the real secret and
	// forwards over HTTP. Its custom DialContext redirects every
	// outbound dial to the mock upstream, so scenarios can name real
	// production hosts (e.g. https://api.github.com) without the
	// harness having to extend the bound-service allowlist.
	mux.Handle("/api/proxy/", nonceMW(newLiteResolver(st, v, scriptSessions, logger, mockAddr)))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// Point both the control rewriter and the credentialed-resolver
	// rewriter at the lite mux so the agent's curl tool_uses against
	// clawvisor.local resolve in-process.
	h.ControlBaseURL = srv.URL
	h.ResolverBaseURL = srv.URL + "/api/proxy"

	workspace := filepath.Join(dataDir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(scn.WorkspaceSource()); err == nil {
		if err := copyDir(scn.WorkspaceSource(), workspace); err != nil {
			return nil, err
		}
	}
	if err := runSetupShell(ctx, workspace, scn.SetupShell); err != nil {
		return nil, err
	}

	var mcpConfigPath string
	if scn.MCPStub {
		path, err := setupMCPStub(ctx, dataDir, workspace)
		if err != nil {
			return nil, fmt.Errorf("mcp stub: %w", err)
		}
		mcpConfigPath = path
	}

	return &Harness{
		Store:         st,
		Endpoint:      srv,
		UserID:        user.ID,
		AgentID:       agent.ID,
		AgentToken:    rawToken,
		Workspace:     workspace,
		Counters:      counters,
		Logger:        logger,
		MockUpstream:  mockUpstream,
		MCPConfigPath: mcpConfigPath,
		recorder:      recorder,
	}, nil
}

// setupMCPStub builds the stub MCP server binary into dataDir and
// writes a workspace-local .mcp.json that points Claude Code at it.
// The stub's MCP_STUB_MARKER_PATH env is wired to
// <workspace>/MCPStubMarkerFilename so a scenario can assert on the
// agent's choice by checking whether that file exists post-step.
//
// Returns the absolute path to the written .mcp.json (suitable for
// --mcp-config). Returns an error if go build fails — surfaced via
// the scenario load path so the failure mode is obvious rather than
// a runtime "MCP server failed to start" inside Claude Code.
func setupMCPStub(ctx context.Context, dataDir, workspace string) (string, error) {
	binPath := filepath.Join(dataDir, "mcpstub")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./drivers/mcpstub")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build stub: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	markerPath := filepath.Join(workspace, MCPStubMarkerFilename)
	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"gmailstub": map[string]any{
				"command": binPath,
				"args":    []string{},
				"env": map[string]string{
					"MCP_STUB_MARKER_PATH": markerPath,
				},
			},
		},
	}
	mcpJSON, err := json.MarshalIndent(mcpConfig, "", "  ")
	if err != nil {
		return "", err
	}
	mcpConfigPath := filepath.Join(dataDir, "mcp-config.json")
	if err := os.WriteFile(mcpConfigPath, mcpJSON, 0o600); err != nil {
		return "", err
	}
	return mcpConfigPath, nil
}

// newMockUpstream returns an httptest server that records every
// incoming request into the counters and looks for one of the
// minted placeholders OR one of the planted real secrets in any
// header value. Either match counts toward
// downstream.placeholder_used — the harness can't tell whether the
// proxy's placeholder swap fired or not (we don't have the resolver
// wired to bump a separate "swap_observed" counter), so a downstream
// call that carries either the placeholder substring (no swap) or
// the planted secret (post-swap) is treated as legitimate use of a
// Clawvisor-supplied credential. A call carrying a string the agent
// invented (e.g. the user-supplied fake `autovault_github_abcdef`)
// matches neither and stays out of the counter.
func newMockUpstream(counters *Counters, placeholderSnapshot func() []string, plantedSecrets []string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counters.Inc(SeriesDownstreamCallsTotal)
		needles := append([]string{}, placeholderSnapshot()...)
		needles = append(needles, plantedSecrets...)
		if requestContainsAnyNeedle(r, needles) {
			counters.Inc(SeriesDownstreamPlaceholderUsed)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return srv
}

// mockUpstreamDialAddr extracts the bare host:port from httptest's
// randomly-assigned URL so the lite resolver's custom DialContext can
// reach the mock without parsing the URL on every request.
func mockUpstreamDialAddr(serverURL string) (string, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	if parsed.Host == "" {
		return "", errors.New("mock upstream URL has no host:port")
	}
	return parsed.Host, nil
}

// requestContainsAnyNeedle is true iff any header value on the
// incoming request contains one of the supplied needles as a
// substring. Substring (not equality) so an `Authorization: Bearer
// <needle>` header still matches.
func requestContainsAnyNeedle(r *http.Request, needles []string) bool {
	if len(needles) == 0 {
		return false
	}
	for _, values := range r.Header {
		for _, v := range values {
			for _, n := range needles {
				if n != "" && strings.Contains(v, n) {
					return true
				}
			}
		}
	}
	return false
}

// EndpointURL is the in-process lite-proxy URL. Drivers append the
// provider-specific path themselves (e.g. /v1/messages).
func (h *Harness) EndpointURL() string {
	return h.Endpoint.URL
}

// seedPreseededTasks writes each PreseededTask into the store as an
// active task owned by the scenario's user+agent, modeling an approval
// the user granted in some prior conversation. Scope-relevant fields
// (purpose, lifetime, expected_tools, expected_egress) come from the
// YAML; everything else is filled in here.
func seedPreseededTasks(ctx context.Context, st store.Store, userID, agentID string, seeds []PreseededTask) error {
	if len(seeds) == 0 {
		return nil
	}
	now := time.Now().UTC()
	for i, seed := range seeds {
		if strings.TrimSpace(seed.Purpose) == "" {
			return fmt.Errorf("preseeded_tasks[%d]: purpose is required", i)
		}
		lifetime := seed.Lifetime
		if lifetime == "" {
			lifetime = "standing"
		}
		schemaVersion := seed.SchemaVersion
		if schemaVersion == 0 {
			schemaVersion = 2
		}
		// Convert through the canonical runtime types so the JSON we
		// store uses the json-tagged shape (`tool_name`, `why`, …)
		// that runtimetasks.EnvelopeFromTask + policy.MatchToolCall
		// expect at scope-check time. Marshalling the YAML-tagged
		// PreseededExpectedTool directly would emit `ToolName`,
		// which the matcher won't see.
		canonTools := make([]runtimetasks.ExpectedTool, 0, len(seed.ExpectedTools))
		for _, t := range seed.ExpectedTools {
			canonTools = append(canonTools, runtimetasks.ExpectedTool{
				ToolName: t.ToolName,
				Why:      t.Why,
			})
		}
		canonEgress := make([]runtimetasks.ExpectedEgress, 0, len(seed.ExpectedEgress))
		for _, e := range seed.ExpectedEgress {
			canonEgress = append(canonEgress, runtimetasks.ExpectedEgress{
				Host: e.Host,
				Why:  e.Why,
			})
		}
		expectedToolsJSON, err := json.Marshal(canonTools)
		if err != nil {
			return fmt.Errorf("preseeded_tasks[%d]: marshal expected_tools: %w", i, err)
		}
		expectedEgressJSON, err := json.Marshal(canonEgress)
		if err != nil {
			return fmt.Errorf("preseeded_tasks[%d]: marshal expected_egress: %w", i, err)
		}
		approvedAt := now
		task := &store.Task{
			UserID:                 userID,
			AgentID:                agentID,
			Purpose:                seed.Purpose,
			Status:                 "active",
			Lifetime:               lifetime,
			ExpectedTools:          expectedToolsJSON,
			ExpectedEgress:         expectedEgressJSON,
			IntentVerificationMode: seed.IntentVerificationMode,
			ExpectedUse:            seed.ExpectedUse,
			SchemaVersion:          schemaVersion,
			ApprovedAt:             &approvedAt,
			ApprovalSource:         "manual",
		}
		if err := st.CreateTask(ctx, task); err != nil {
			return fmt.Errorf("preseeded_tasks[%d]: %w", i, err)
		}
	}
	return nil
}

// CountActiveTasksForAgent reports how many active tasks belong to the
// harness's agent. Useful as a ground-truth check on approval counts.
func (h *Harness) CountActiveTasksForAgent(ctx context.Context) (int, error) {
	tasks, _, err := h.Store.ListTasks(ctx, h.UserID, store.TaskFilter{ActiveOnly: true})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tasks {
		if t.AgentID == h.AgentID {
			n++
		}
	}
	return n, nil
}

// buildJudgeConfig returns a VerificationConfig wired against the
// first available LLM key. Anthropic takes precedence because Haiku
// is the canonical judge model; OpenAI is the fallback for runs that
// only carry an OpenAI key. Returns ok=false when neither key is
// set, so the caller leaves the judge unwired.
func buildJudgeConfig(keys Keys) (config.VerificationConfig, bool) {
	switch {
	case keys.Anthropic != "":
		return config.VerificationConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:        true,
				Provider:       "anthropic",
				Endpoint:       "https://api.anthropic.com/v1",
				APIKey:         keys.Anthropic,
				Model:          "claude-haiku-4-5-20251001",
				TimeoutSeconds: 15,
			},
		}, true
	case keys.OpenAI != "":
		return config.VerificationConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:        true,
				Provider:       "openai",
				Endpoint:       "https://api.openai.com/v1",
				APIKey:         keys.OpenAI,
				Model:          "gpt-5-mini",
				TimeoutSeconds: 15,
			},
		}, true
	}
	return config.VerificationConfig{}, false
}

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
