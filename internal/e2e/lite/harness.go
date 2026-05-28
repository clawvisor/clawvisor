package lite

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	logger := slog.New(slog.NewTextHandler(testLogWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cfgPtr := config.Default()
	cfg := *cfgPtr
	// Empty adapter registry — scenarios in this harness exercise local
	// tools (Bash/Read/Write/Edit), not credentialed gateway adapters.
	reg := adapters.NewRegistry()
	hub := events.NewHub()
	assessor := taskrisk.NoopAssessor{}

	tasksHandler := handlers.NewTasksHandler(st, v, reg, nil /*notify*/, cfg, logger, "" /*baseURL*/, hub, assessor)

	h := handlers.NewLLMEndpointHandler(st, v, logger)
	h.Forwarder = llmproxy.NewForwarder(v)
	h.Inspector = inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	h.InlineTaskCreator = tasksHandler
	h.TaskScope = llmproxy.NewStoreTaskScopeChecker(st)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))
	mux.Handle("POST /v1/messages/count_tokens", mw(http.HandlerFunc(h.Messages)))
	mux.Handle("POST /v1/chat/completions", mw(http.HandlerFunc(h.ChatCompletions)))
	mux.Handle("POST /v1/responses", mw(http.HandlerFunc(h.Responses)))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// ControlBaseURL needs to be syntactically valid; the inline-approval
	// intercept fires before rewrite for our scenarios, so this URL is
	// only used for the non-inline fallthrough path we don't exercise.
	h.ControlBaseURL = srv.URL

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
		Store:      st,
		Endpoint:   srv,
		UserID:     user.ID,
		AgentID:    agent.ID,
		AgentToken: rawToken,
		Workspace:  workspace,
		Counters:   NewCounters(),
		Logger:     logger,
	}, nil
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
