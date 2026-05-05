// Package harness wires an in-process clawvisor runtime stack for the e2e
// LLM-driven harness: sqlite store + local vault + runtime proxy server +
// RuntimeHandler + a Clawvisor API mux (Tasks / Approvals / Gateway) mounted
// behind the synthetic upstream APIHost. The harness reuses production code
// paths, so a scenario run exercises the same authenticator, session guard,
// egress policy, and task/gateway handlers a real deploy would.
//
// No HTTP listener is bound for the API. The mux is registered with the
// Upstreams layer so the responder can call https://APIHost/api/... over
// the runtime proxy's MITM exactly as it calls every other test upstream;
// requests inbound to that handler get the seeded principal injected by
// testAuthMiddleware instead of going through JWT/agent-token auth.
//
// Approval resolution is invoked in-process by ResolveApproval, which
// dispatches by approval kind and transport: runtime proxy review one-offs
// → RuntimeHandler.ResolveApproval; gateway-routed request_once →
// ApprovalsHandler.Approve / .Deny; task_create / task_expand →
// TasksHandler.Approve / .Deny / .ExpandApprove / .ExpandDeny.
package harness

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/clawvisor/clawvisor/internal/api/handlers"
	runtimeproxy "github.com/clawvisor/clawvisor/pkg/runtime/proxy"
	runtimereview "github.com/clawvisor/clawvisor/pkg/runtime/review"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/internal/vault"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Server is the booted harness state. Tests get one Server per scenario.
type Server struct {
	DataDir string
	Config  *config.Config

	DB    *sql.DB
	Store store.Store
	Vault *intvault.LocalVault

	Proxy       *runtimeproxy.Server
	Manager     *runtimeproxy.Manager
	Handler     *handlers.RuntimeHandler
	ReviewCache runtimereview.HeldApprovalCache

	Upstreams *Upstreams

	// API is the in-process Clawvisor API mux (tasks, approvals, gateway)
	// the harness exposes as a routable upstream at APIHost. Optional —
	// scenarios that don't reference api.clawvisor.test won't trigger it.
	API *APISurface

	logger *slog.Logger

	principalMu sync.Mutex
	principal   *Principal
}

// Start boots the harness in dataDir. The directory must already exist;
// callers typically pass t.TempDir().
func Start(ctx context.Context, dataDir string, logger *slog.Logger) (*Server, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("harness: dataDir is required")
	}

	db, err := sqlite.New(ctx, filepath.Join(dataDir, "harness.db"))
	if err != nil {
		return nil, fmt.Errorf("harness: open sqlite: %w", err)
	}
	st := sqlite.NewStore(db)

	// 32 zero bytes is fine here — vault contents are scenario-only and the
	// database is wiped at the end of each run.
	vault, err := intvault.NewLocalVaultFromKeyWithDB(make([]byte, 32), db, "sqlite")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("harness: build vault: %w", err)
	}

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = filepath.Join(dataDir, "proxy")
	cfg.RuntimeProxy.ListenAddr = "127.0.0.1:0"
	cfg.RuntimeProxy.TLS = false

	proxy, err := runtimeproxy.NewServer(runtimeproxy.Config{
		DataDir: cfg.RuntimeProxy.DataDir,
		Addr:    cfg.RuntimeProxy.ListenAddr,
	}, logger)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("harness: new proxy: %w", err)
	}

	// Mirror pkg/clawvisor/run.go install order. The harness skips
	// observe-notice scrubbing, inbound-secret capture, placeholder swap,
	// tool-use interceptors, context judges, and timing traces — the policy
	// layer is what scenarios assert against. Add hooks back as scenarios
	// need them.
	proxy.InstallSessionGuard(&runtimeproxy.Authenticator{Store: st, Config: cfg, Logger: logger})
	proxy.InstallRequestContextCarrier()
	proxy.InstallEgressPolicy(runtimeproxy.PolicyHooks{
		Store:  st,
		Config: cfg,
		Logger: logger,
	})

	if err := proxy.Start(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("harness: start proxy: %w", err)
	}

	manager := &runtimeproxy.Manager{
		Store:  st,
		Config: cfg,
		Logger: logger,
		Proxy:  proxy,
	}
	reviewCache := runtimereview.NewApprovalCache()
	handler := handlers.NewRuntimeHandler(st, vault, manager, cfg, reviewCache)

	upstreams, err := NewUpstreams(proxy)
	if err != nil {
		_ = proxy.Shutdown(ctx)
		_ = db.Close()
		return nil, fmt.Errorf("harness: build upstreams: %w", err)
	}

	srv := &Server{
		DataDir:     dataDir,
		Config:      cfg,
		DB:          db,
		Store:       st,
		Vault:       vault,
		Proxy:       proxy,
		Manager:     manager,
		Handler:     handler,
		ReviewCache: reviewCache,
		Upstreams:   upstreams,
		logger:      logger,
	}
	srv.API = buildAPISurface(srv, logger)
	upstreams.AddHandler(APIHost, srv.API)
	return srv, nil
}

// Stop tears the harness down. Safe to call multiple times.
func (s *Server) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if s.Upstreams != nil {
		s.Upstreams.Close()
	}
	if s.Proxy != nil {
		_ = s.Proxy.Shutdown(ctx)
	}
	if s.DB != nil {
		return s.DB.Close()
	}
	return nil
}

// ProxyURL returns the http://host:port the runtime proxy is listening on.
func (s *Server) ProxyURL() string {
	if s.Manager == nil {
		return ""
	}
	return s.Manager.ProxyURL()
}
