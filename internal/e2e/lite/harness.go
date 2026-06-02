package lite

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api/handlers"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
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

	// recorder is the InlineTaskCreator wrapper used by the mock
	// upstream to recognize minted placeholders. Exposed so test
	// helpers can also inspect what was minted.
	recorder *recordingInlineCreator
}

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
	// Catalog gives the lite-proxy a way to reverse-resolve the
	// (host, method, path) of a credentialed call to a known
	// (service, action). Without this the scope-drift menu is
	// unreachable in the harness because the path that mints drifts
	// requires `cfg.Catalog != nil && cfg.TaskScope != nil`. The
	// scenario YAML's `service_catalog:` block names which built-in
	// catalogs to load (today only "github" is defined); without it
	// the catalog stays empty and tests behave as before.
	defs, defsErr := scenarioServiceCatalogDefs(scn)
	if defsErr != nil {
		return nil, defsErr
	}
	if len(defs) > 0 {
		h.Catalog = llmproxy.NewLazyServiceCatalog(defs)
	}
	// Replace the default in-process scope-drift registry with a
	// counting wrapper so scenario expectations can assert on drift
	// lifecycle events (minted, option chosen, outcome, pre-clear).
	// NewLLMEndpointHandler already populated h.ScopeDrifts with a
	// memory registry; the wrapper forwards to it.
	h.ScopeDrifts = NewCountingScopeDriftRegistry(h.ScopeDrifts, counters)

	// Optional: live intent verifier. Without the right env vars
	// (CLAWVISOR_E2E_VERIFIER_PROVIDER=gemini etc.) this stays nil
	// and the lite-proxy skips intent verification — drifts can only
	// originate from the task_scope check. With Gemini ADC available,
	// the verifier runs against gemini-3.1-flash-lite-preview by
	// default and the /justify path becomes reachable.
	verifier, _, vErr := verifierConfigFromEnv(logger)
	if vErr != nil {
		return nil, vErr
	}
	if verifier != nil {
		h.IntentVerifier = llmproxy.NewIntentVerifierAdapter(verifier)
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
	nonceMW := middleware.RequireAgentLLMNonce(st, h.CallerNonces, logger)
	listVaultItems := nonceMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counters.Inc(SeriesVaultItemsListed)
		vaultHandler.ListForAgent(w, r)
	}))
	mux.Handle("GET /api/control/vault/items", listVaultItems)

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
	mux.Handle("/api/proxy/", nonceMW(newLiteResolver(st, v, logger, mockAddr)))

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

	return &Harness{
		Store:        st,
		Endpoint:     srv,
		UserID:       user.ID,
		AgentID:      agent.ID,
		AgentToken:   rawToken,
		Workspace:    workspace,
		Counters:     counters,
		Logger:       logger,
		MockUpstream: mockUpstream,
		recorder:     recorder,
	}, nil
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

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
