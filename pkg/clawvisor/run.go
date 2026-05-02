package clawvisor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/clawvisor/clawvisor/internal/api"
	"github.com/clawvisor/clawvisor/internal/api/handlers"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/llm"
	runtimeleases "github.com/clawvisor/clawvisor/internal/runtime/leases"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	runtimeproxy "github.com/clawvisor/clawvisor/internal/runtime/proxy"
	runtimereview "github.com/clawvisor/clawvisor/internal/runtime/review"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// RunWithContext starts the Clawvisor server using the provided context for
// lifecycle management. The caller is responsible for cancellation and signal
// handling. Used by the daemon to control server lifetime during first-run
// service setup (where the server may need to be restarted).
func RunWithContext(ctx context.Context, opts *ServerOptions) error {
	var apiOpts []api.ServerOption
	var runtimeSrv *runtimeproxy.Server
	var runtimeMgr *runtimeproxy.Manager
	var reviewCache runtimereview.HeldApprovalCache

	if opts.Config != nil && opts.Config.RuntimeProxy.Enabled {
		dataDir := opts.Config.RuntimeProxy.DataDir
		if home, err := os.UserHomeDir(); err == nil && len(dataDir) > 1 && dataDir[:2] == "~/" {
			dataDir = filepath.Join(home, dataDir[2:])
		}
		timingTraceDir := opts.Config.RuntimeProxy.TimingTraceDir
		if home, err := os.UserHomeDir(); err == nil && len(timingTraceDir) > 1 && timingTraceDir[:2] == "~/" {
			timingTraceDir = filepath.Join(home, timingTraceDir[2:])
		}
		bodyTraceDir := opts.Config.RuntimeProxy.BodyTraceDir
		if home, err := os.UserHomeDir(); err == nil && len(bodyTraceDir) > 1 && bodyTraceDir[:2] == "~/" {
			bodyTraceDir = filepath.Join(home, bodyTraceDir[2:])
		}
		dashboardBaseURL := strings.TrimSpace(opts.Config.Server.PublicURL)
		if dashboardBaseURL == "" && opts.Config.Server.Port != 0 {
			host := strings.TrimSpace(opts.Config.Server.Host)
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			dashboardBaseURL = fmt.Sprintf("http://%s:%d", host, opts.Config.Server.Port)
		}
		var err error
		runtimeSrv, err = runtimeproxy.NewServer(runtimeproxy.Config{
			DataDir:           dataDir,
			Addr:              opts.Config.RuntimeProxy.ListenAddr,
			TLS:               opts.Config.RuntimeProxy.TLS,
			DashboardBaseURL:  dashboardBaseURL,
			ListenerHostnames: opts.Config.RuntimeProxy.ListenerHostnames,
			LogTimings:        opts.Config.RuntimeProxy.TimingTraceEnabled,
			TimingTraceDir:    timingTraceDir,
			BodyTraces:        opts.Config.RuntimeProxy.BodyTraceEnabled,
			BodyTraceDir:      bodyTraceDir,
			RedisClient:       opts.RedisClient,
		}, opts.Logger)
		if err != nil {
			return err
		}
		if opts.Config.RuntimeProxy.TimingTraceEnabled && opts.Logger != nil {
			traceDir := timingTraceDir
			if traceDir == "" {
				traceDir = filepath.Join(dataDir, "timing-traces")
			}
			opts.Logger.Info("runtime proxy timing traces enabled", "dir", traceDir)
		}
		if opts.Config.RuntimeProxy.BodyTraceEnabled && opts.Logger != nil {
			traceDir := bodyTraceDir
			if traceDir == "" {
				traceDir = filepath.Join(dataDir, "body-traces")
			}
			opts.Logger.Info("runtime proxy body traces enabled", "dir", traceDir)
		}
		runtimeSrv.InstallSessionGuard(&runtimeproxy.Authenticator{Store: opts.Store, Config: opts.Config, Logger: opts.Logger})
		runtimeSrv.InstallObserveNoticeRequestScrubber()
		runtimeSrv.InstallInboundSecretCapture(runtimeproxy.InboundSecretHooks{
			Store:  opts.Store,
			Vault:  opts.Vault,
			Config: opts.Config,
			Logger: opts.Logger,
		})
		runtimeSrv.InstallRequestContextCarrier()
		runtimeSrv.InstallPlaceholderSwap(runtimeproxy.PlaceholderHooks{
			Store:  opts.Store,
			Vault:  opts.Vault,
			Config: opts.Config,
		})
		reviewCache = runtimereview.NewApprovalCache()
		if opts.RedisClient != nil {
			reviewCache = runtimereview.NewRedisApprovalCache(opts.RedisClient)
		}
		contextJudge := runtimepolicy.NewLLMRuntimeContextJudge(llm.NewHealth(opts.Config.LLM), opts.Logger)
		runtimeSrv.InstallToolUseInterceptors(runtimeproxy.ToolUseHooks{
			Store:       opts.Store,
			Config:      opts.Config,
			ReviewCache: reviewCache,
			Leases: runtimeleases.Service{
				Store: opts.Store,
			},
			ContextJudge: contextJudge,
		})
		runtimeSrv.InstallEgressPolicy(runtimeproxy.PolicyHooks{
			Store:        opts.Store,
			Config:       opts.Config,
			Logger:       opts.Logger,
			ContextJudge: contextJudge,
		})
		runtimeSrv.InstallTimingTrace()
		runtimeMgr = &runtimeproxy.Manager{
			Store:  opts.Store,
			Config: opts.Config,
			Logger: opts.Logger,
			Proxy:  runtimeSrv,
		}
	}

	apiOpts = append(apiOpts, api.WithFeatures(api.FeatureSet{
		MultiTenant:       opts.Features.MultiTenant,
		EmailVerification: opts.Features.EmailVerification,
		Passkeys:          opts.Features.Passkeys,
		SSO:               opts.Features.SSO,
		Teams:             opts.Features.Teams,
		UsageMetering:     opts.Features.UsageMetering,
		PasswordAuth:      opts.Features.PasswordAuth,
		Billing:           opts.Features.Billing,
		LocalDaemon:       opts.Features.LocalDaemon,
		RuntimeProxy:      opts.Features.RuntimeProxy,
		SecretVault:       opts.Features.SecretVault,
		RuntimePolicyUI:   opts.Features.RuntimePolicyUI,
		RuntimeActivity:   opts.Features.RuntimeActivity,
		AgentLiveSessions: opts.Features.AgentLiveSessions,
		ServicePresets:    opts.Features.ServicePresets,
	}))

	apiOpts = append(apiOpts, api.WithExtraRoutes(func(mux *http.ServeMux, deps api.Dependencies) {
		if runtimeMgr != nil {
			runtimeHandler := handlers.NewRuntimeHandler(deps.Store, deps.Vault, runtimeMgr, opts.Config, reviewCache)
			user := middleware.RequireUser(deps.JWTService, deps.Store)
			agent := middleware.RequireAgent(deps.Store)
			mux.Handle("POST /api/runtime/sessions", agent(http.HandlerFunc(runtimeHandler.CreateSession)))
			mux.Handle("POST /api/runtime/placeholders", agent(http.HandlerFunc(runtimeHandler.CreatePlaceholder)))
			mux.Handle("GET /api/runtime/placeholders", user(http.HandlerFunc(runtimeHandler.ListUserPlaceholders)))
			mux.Handle("POST /api/runtime/placeholders/mint", user(http.HandlerFunc(runtimeHandler.CreateUserPlaceholder)))
			mux.Handle("DELETE /api/runtime/placeholders/{placeholder}", user(http.HandlerFunc(runtimeHandler.DeleteUserPlaceholder)))
			mux.Handle("GET /api/runtime/sessions", user(http.HandlerFunc(runtimeHandler.ListSessions)))
			mux.Handle("POST /api/runtime/sessions/{id}/revoke", user(http.HandlerFunc(runtimeHandler.RevokeSession)))
			mux.Handle("GET /api/runtime/status", user(http.HandlerFunc(runtimeHandler.Status)))
			mux.Handle("GET /api/runtime/approvals", user(http.HandlerFunc(runtimeHandler.ListApprovals)))
			mux.Handle("POST /api/runtime/approvals/{id}/resolve", user(http.HandlerFunc(runtimeHandler.ResolveApproval)))
			mux.Handle("GET /api/runtime/events", user(http.HandlerFunc(runtimeHandler.ListEvents)))
			mux.Handle("GET /api/runtime/events/{id}/rule-candidate", user(http.HandlerFunc(runtimeHandler.GetRuleCandidateForEvent)))
			mux.Handle("POST /api/runtime/events/{id}/promote-task", user(http.HandlerFunc(runtimeHandler.PromoteEventToTask)))
			mux.Handle("GET /api/runtime/leases", user(http.HandlerFunc(runtimeHandler.ListLeases)))
			mux.Handle("GET /api/runtime/rules", user(http.HandlerFunc(runtimeHandler.ListRules)))
			mux.Handle("POST /api/runtime/rules", user(http.HandlerFunc(runtimeHandler.CreateRule)))
			mux.Handle("GET /api/runtime/rules/{id}", user(http.HandlerFunc(runtimeHandler.GetRule)))
			mux.Handle("PUT /api/runtime/rules/{id}", user(http.HandlerFunc(runtimeHandler.UpdateRule)))
			mux.Handle("DELETE /api/runtime/rules/{id}", user(http.HandlerFunc(runtimeHandler.DeleteRule)))
			mux.Handle("GET /api/runtime/starter-profiles", user(http.HandlerFunc(runtimeHandler.ListStarterProfiles)))
			mux.Handle("POST /api/runtime/starter-profiles/{profile}/apply", user(http.HandlerFunc(runtimeHandler.ApplyStarterProfile)))
			mux.Handle("GET /api/runtime/preset-decisions", user(http.HandlerFunc(runtimeHandler.GetPresetDecision)))
			mux.Handle("PUT /api/runtime/preset-decisions", user(http.HandlerFunc(runtimeHandler.UpsertPresetDecision)))
		}
		if opts.ExtraRoutes != nil {
			opts.ExtraRoutes(mux, Dependencies{
				Store:      deps.Store,
				Vault:      deps.Vault,
				JWTService: deps.JWTService,
				AdapterReg: deps.AdapterReg,
				Notifier:   deps.Notifier,
				Logger:     deps.Logger,
				BaseURL:    deps.BaseURL,
			})
		}
	}))

	if opts.WrapRoutes != nil {
		apiOpts = append(apiOpts, api.WithWrapRoutes(opts.WrapRoutes))
	}

	if opts.SkipBuiltinAuth {
		apiOpts = append(apiOpts, api.WithSkipBuiltinAuth())
	}

	if opts.Quiet {
		apiOpts = append(apiOpts, api.WithQuiet())
	}

	if opts.X25519Key != nil {
		apiOpts = append(apiOpts, api.WithE2EKey(opts.X25519Key))
	}

	if opts.Config.Relay.DaemonID != "" {
		apiOpts = append(apiOpts, api.WithDaemonKeys(
			opts.Config.Relay.DaemonID,
			opts.X25519Key,
		))
	}

	if opts.PushNotifier != nil {
		apiOpts = append(apiOpts, api.WithPushNotifier(opts.PushNotifier))
	}

	if opts.MessageBuffer != nil {
		apiOpts = append(apiOpts, api.WithGroupChatBuffer(opts.MessageBuffer))
	}

	if opts.EventHub != nil {
		apiOpts = append(apiOpts, api.WithEventHub(opts.EventHub))
	}

	if opts.DecisionBus != nil {
		apiOpts = append(apiOpts, api.WithDecisionBus(opts.DecisionBus))
	}

	if opts.AdapterGenFactory != nil {
		apiOpts = append(apiOpts, api.WithAdapterGenFactory(opts.AdapterGenFactory))
	}

	if opts.GatewayHooks != nil {
		apiOpts = append(apiOpts, api.WithGatewayHooks(&api.GatewayHooks{
			BeforeAuthorize: opts.GatewayHooks.BeforeAuthorize,
		}))
	}

	if opts.FeedbackHooks != nil {
		apiOpts = append(apiOpts, api.WithFeedbackHooks(&api.FeedbackHooks{
			AfterBugReport: opts.FeedbackHooks.AfterBugReport,
		}))
	}

	// Multi-instance Redis-backed stores.
	if opts.TicketStore != nil {
		apiOpts = append(apiOpts, api.WithTicketStore(opts.TicketStore))
	}
	if opts.ReplayCache != nil {
		apiOpts = append(apiOpts, api.WithReplayCache(opts.ReplayCache))
	}
	if opts.TokenCache != nil {
		apiOpts = append(apiOpts, api.WithTokenCache(opts.TokenCache))
	}
	if opts.DevicePairingStore != nil {
		apiOpts = append(apiOpts, api.WithDevicePairingStore(opts.DevicePairingStore))
	}
	if opts.OAuthStateStore != nil {
		apiOpts = append(apiOpts, api.WithOAuthStateStore(opts.OAuthStateStore))
	}
	if opts.PairingCodeStore != nil {
		apiOpts = append(apiOpts, api.WithPairingCodeStore(opts.PairingCodeStore))
	}
	if opts.DedupCache != nil {
		apiOpts = append(apiOpts, api.WithDedupCache(opts.DedupCache))
	}
	if opts.VerdictCache != nil {
		apiOpts = append(apiOpts, api.WithVerdictCache(opts.VerdictCache))
	}
	if opts.ExtractionTracker != nil {
		apiOpts = append(apiOpts, api.WithExtractionTracker(opts.ExtractionTracker))
	}
	if opts.LocalServiceProvider != nil {
		apiOpts = append(apiOpts, api.WithLocalServiceProvider(&localSvcAdapter{opts.LocalServiceProvider}))
	}
	if opts.LocalServiceExecutor != nil {
		apiOpts = append(apiOpts, api.WithLocalServiceExecutor(&localExecAdapter{opts.LocalServiceExecutor}))
	}

	srv, err := api.New(
		opts.Config, opts.Store, opts.Vault, opts.JWTService,
		opts.AdapterReg, opts.Notifier, opts.Config.LLM, opts.MagicStore,
		apiOpts...,
	)
	if err != nil {
		return err
	}

	if runtimeSrv != nil {
		if err := runtimeSrv.Start(); err != nil {
			return err
		}
		opts.Logger.Info("runtime proxy running", "addr", runtimeSrv.Addr())
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := runtimeSrv.Shutdown(shutdownCtx); err != nil && opts.Logger != nil {
				opts.Logger.Warn("runtime proxy shutdown failed", "error", err)
			}
		}()
	}

	// Start relay client if configured. Give it the real server handler so
	// relay-proxied requests go through the full middleware stack.
	if opts.RelayClient != nil {
		opts.RelayClient.SetHandler(srv.Handler())
		go func() {
			if err := opts.RelayClient.Run(ctx); err != nil && ctx.Err() == nil {
				opts.Logger.Error("relay client stopped", "error", err)
			}
		}()
	}

	return srv.Run(ctx)
}

// Run starts the Clawvisor server with the given options and blocks until
// interrupted (SIGINT/SIGTERM).
func Run(opts *ServerOptions) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return RunWithContext(ctx, opts)
}

// localSvcAdapter wraps the public LocalServiceProvider to implement
// handlers.LocalServiceProvider by converting between type systems.
type localSvcAdapter struct {
	inner LocalServiceProvider
}

func (a *localSvcAdapter) ActiveLocalServices(ctx context.Context, userID string) ([]handlers.LocalCatalogService, error) {
	svcs, err := a.inner.ActiveLocalServices(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]handlers.LocalCatalogService, len(svcs))
	for i, s := range svcs {
		actions := make([]handlers.LocalCatalogAction, len(s.Actions))
		for j, act := range s.Actions {
			params := make([]handlers.LocalCatalogParam, len(act.Params))
			for k, p := range act.Params {
				params[k] = handlers.LocalCatalogParam{
					Name: p.Name, Type: p.Type,
					Required: p.Required, Description: p.Description,
				}
			}
			actions[j] = handlers.LocalCatalogAction{
				ID: act.ID, Name: act.Name,
				Description: act.Description, Params: params,
			}
		}
		out[i] = handlers.LocalCatalogService{
			ServiceID: s.ServiceID, DaemonName: s.DaemonName,
			Name: s.Name, Description: s.Description,
			Actions: actions,
		}
	}
	return out, nil
}

// localExecAdapter wraps the public LocalServiceExecutor to implement
// handlers.LocalServiceExecutor. Since both use *adapters.Result, no
// type conversion is needed.
type localExecAdapter struct {
	inner LocalServiceExecutor
}

func (a *localExecAdapter) Execute(ctx context.Context, userID, service, action string, params map[string]any) (*adapters.Result, error) {
	return a.inner.Execute(ctx, userID, service, action, params)
}
