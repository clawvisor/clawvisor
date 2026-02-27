package api

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/clawvisor/clawvisor/internal/adapters"
	"github.com/clawvisor/clawvisor/internal/api/handlers"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/config"
	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/internal/notify"
	"github.com/clawvisor/clawvisor/internal/ratelimit"
	"github.com/clawvisor/clawvisor/internal/store"
	"github.com/clawvisor/clawvisor/internal/vault"
	skillfiles "github.com/clawvisor/clawvisor/skills"

	"golang.org/x/time/rate"
)

// Server is the Clawvisor HTTP server.
type Server struct {
	cfg        *config.Config
	store      store.Store
	vault      vault.Vault
	jwtSvc     *auth.JWTService
	adapterReg *adapters.Registry
	notifier   notify.Notifier
	llmCfg     config.LLMConfig
	logger     *slog.Logger
	http       *http.Server

	magicStore *auth.MagicTokenStore

	// approvalsCleaner is used to stop the background goroutine.
	approvalsHandler *handlers.ApprovalsHandler
	tasksHandler     *handlers.TasksHandler
}

// New creates a Server and registers all routes.
// magicStore may be nil when magic link auth is not enabled.
func New(
	cfg *config.Config,
	st store.Store,
	v vault.Vault,
	jwtSvc *auth.JWTService,
	adapterReg *adapters.Registry,
	notifier notify.Notifier,
	llmCfg config.LLMConfig,
	magicStore *auth.MagicTokenStore,
) (*Server, error) {
	logOpts := &slog.HandlerOptions{Level: cfg.Server.SlogLevel()}
	var logHandler slog.Handler
	switch {
	case cfg.Server.LogFormat == "json":
		logHandler = slog.NewJSONHandler(os.Stdout, logOpts)
	case cfg.Server.LogFormat == "text":
		logHandler = slog.NewTextHandler(os.Stdout, logOpts)
	case !cfg.Server.IsLocal():
		logHandler = slog.NewJSONHandler(os.Stdout, logOpts)
	default:
		logHandler = slog.NewTextHandler(os.Stdout, logOpts)
	}
	logger := slog.New(logHandler)

	s := &Server{
		cfg:        cfg,
		store:      st,
		vault:      v,
		jwtSvc:     jwtSvc,
		adapterReg: adapterReg,
		notifier:   notifier,
		llmCfg:     llmCfg,
		magicStore: magicStore,
		logger:     logger,
	}

	mux := s.routes()

	s.http = &http.Server{
		Addr:         cfg.Server.Addr(),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return s, nil
}

// routes builds the HTTP mux with all registered handlers.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Public URL for links in notifications and OAuth redirects.
	// Falls back to a URL derived from the bind address.
	baseURL := s.cfg.Server.PublicURL
	if baseURL == "" {
		baseHost := s.cfg.Server.Host
		if baseHost == "0.0.0.0" || baseHost == "127.0.0.1" || baseHost == "" {
			baseHost = "localhost"
		}
		baseURL = fmt.Sprintf("http://%s:%d", baseHost, s.cfg.Server.Port)
	}

	// Handlers
	authHandler := handlers.NewAuthHandler(s.jwtSvc, s.store, s.cfg.Auth, s.magicStore, baseURL)
	authMode := "password"
	if s.magicStore != nil {
		authMode = "magic_link"
	}
	healthHandler := handlers.NewHealthHandler(s.store, s.vault, authMode)
	restrictionsHandler := handlers.NewRestrictionsHandler(s.store)
	agentsHandler := handlers.NewAgentsHandler(s.store)
	auditHandler := handlers.NewAuditHandler(s.store)
	// The Telegram notifier also implements TelegramPairer for the pairing flow.
	var pairer notify.TelegramPairer
	if p, ok := s.notifier.(notify.TelegramPairer); ok {
		pairer = p
	}
	notificationsHandler := handlers.NewNotificationsHandler(s.store, s.notifier, pairer)
	// Construct intent verifier (noop if disabled).
	var verifier intent.Verifier = intent.NoopVerifier{}
	if s.llmCfg.Verification.Enabled {
		verifier = intent.NewLLMVerifier(s.llmCfg.Verification)
	}

	gatewayHandler := handlers.NewGatewayHandler(
		s.store, s.vault, s.adapterReg,
		s.notifier, verifier, *s.cfg, s.logger, baseURL,
	)
	servicesHandler := handlers.NewServicesHandler(s.store, s.vault, s.adapterReg, s.logger, baseURL)
	skillHandler := handlers.NewSkillHandler(s.store, s.vault, s.adapterReg, s.logger)
	approvalsHandler := handlers.NewApprovalsHandler(s.store, s.vault, s.adapterReg, s.notifier, s.logger)
	s.approvalsHandler = approvalsHandler
	tasksHandler := handlers.NewTasksHandler(s.store,
		s.notifier, *s.cfg, s.logger, baseURL)
	s.tasksHandler = tasksHandler

	// Middleware
	requireUser := middleware.RequireUser(s.jwtSvc, s.store)
	requireAgent := middleware.RequireAgent(s.store)
	logMiddleware := middleware.Logging(s.logger)
	securityMiddleware := middleware.Security(s.cfg.Server.IsLocal())

	// Rate limiters (skip when config is zero-valued, e.g. in tests)
	rlCfg := s.cfg.RateLimit
	gatewayRL := newKeyedLimiterFromBucket(rlCfg.Gateway)
	oauthRL := newKeyedLimiterFromBucket(rlCfg.OAuth)
	policyRL := newKeyedLimiterFromBucket(rlCfg.PolicyAPI)

	agentKeyFn := func(r *http.Request) string {
		if a := middleware.AgentFromContext(r.Context()); a != nil {
			return a.ID
		}
		return ""
	}
	userKeyFn := func(r *http.Request) string {
		if u := middleware.UserFromContext(r.Context()); u != nil {
			return u.ID
		}
		return ""
	}

	user := func(h http.HandlerFunc) http.Handler { return requireUser(h) }
	agent := func(h http.HandlerFunc) http.Handler { return requireAgent(h) }

	// Rate-limited route helpers: auth runs first (resolves identity), then rate limit checks.
	agentRateLimited := func(h http.HandlerFunc) http.Handler {
		return requireAgent(middleware.RateLimit(gatewayRL, agentKeyFn, rlCfg.Gateway.Limit)(h))
	}
	userOAuthRL := func(h http.HandlerFunc) http.Handler {
		return requireUser(middleware.RateLimit(oauthRL, userKeyFn, rlCfg.OAuth.Limit)(h))
	}
	userPolicyRL := func(h http.HandlerFunc) http.Handler {
		return requireUser(middleware.RateLimit(policyRL, userKeyFn, rlCfg.PolicyAPI.Limit)(h))
	}

	// Health (no auth)
	mux.HandleFunc("GET /health", healthHandler.Health)
	mux.HandleFunc("GET /ready", healthHandler.Ready)
	mux.HandleFunc("GET /api/config/public", healthHandler.ConfigPublic)

	// Auth (no auth required)
	mux.HandleFunc("POST /api/auth/register", authHandler.Register)
	mux.HandleFunc("POST /api/auth/login", authHandler.Login)
	mux.HandleFunc("POST /api/auth/refresh", authHandler.Refresh)

	// Magic link auth (no auth required; only registered when magicStore is set)
	if s.magicStore != nil {
		mux.HandleFunc("GET /auth/local", authHandler.HandleMagicLink)
	}

	// Auth (requires user JWT)
	mux.Handle("POST /api/auth/logout", user(authHandler.Logout))
	mux.Handle("GET /api/me", user(authHandler.Me))
	mux.Handle("PUT /api/me", user(authHandler.UpdateMe))
	mux.Handle("DELETE /api/me", user(authHandler.DeleteMe))

	// Restrictions (rate-limited writes)
	mux.Handle("GET /api/restrictions", user(restrictionsHandler.List))
	mux.Handle("POST /api/restrictions", userPolicyRL(restrictionsHandler.Create))
	mux.Handle("DELETE /api/restrictions/{id}", userPolicyRL(restrictionsHandler.Delete))

	// Agents (user JWT)
	mux.Handle("GET /api/agents", user(agentsHandler.List))
	mux.Handle("POST /api/agents", user(agentsHandler.Create))
	mux.Handle("DELETE /api/agents/{id}", user(agentsHandler.Delete))

	// Notifications (user JWT)
	mux.Handle("GET /api/notifications", user(notificationsHandler.List))
	mux.Handle("PUT /api/notifications/telegram", user(notificationsHandler.UpsertTelegram))
	mux.Handle("DELETE /api/notifications/telegram", user(notificationsHandler.DeleteTelegram))
	mux.Handle("POST /api/notifications/telegram/test", user(notificationsHandler.TestTelegram))
	mux.Handle("POST /api/notifications/telegram/pair", user(notificationsHandler.StartPairing))
	mux.Handle("GET /api/notifications/telegram/pair/{pairing_id}", user(notificationsHandler.PairingStatus))
	mux.Handle("POST /api/notifications/telegram/pair/{pairing_id}/confirm", user(notificationsHandler.ConfirmPairing))

	// Gateway (agent token, rate-limited)
	mux.Handle("POST /api/gateway/request", agentRateLimited(gatewayHandler.HandleRequest))
	mux.Handle("GET /api/gateway/request/{request_id}/status", agentRateLimited(gatewayHandler.HandleStatus))

	// Services / OAuth (user JWT, rate-limited)
	mux.Handle("GET /api/services", user(servicesHandler.List))
	mux.Handle("GET /api/oauth/url", userOAuthRL(servicesHandler.OAuthGetURL))     // fetch → returns {"url":"..."}
	mux.Handle("GET /api/oauth/start", userOAuthRL(servicesHandler.OAuthStart))    // kept for compat
	mux.HandleFunc("GET /api/oauth/callback", servicesHandler.OAuthCallback) // no auth: browser redirect
	mux.Handle("POST /api/services/{serviceID}/activate", user(servicesHandler.Activate))
	mux.Handle("POST /api/services/{serviceID}/activate-key", user(servicesHandler.ActivateWithKey))
	mux.Handle("POST /api/services/{serviceID}/deactivate", user(servicesHandler.Deactivate))

	// Skill catalog (agent token)
	mux.Handle("GET /api/skill/catalog", agent(skillHandler.Catalog))

	// Approvals (user JWT)
	mux.Handle("GET /api/approvals", user(approvalsHandler.List))
	mux.Handle("POST /api/approvals/{request_id}/approve", user(approvalsHandler.Approve))
	mux.Handle("POST /api/approvals/{request_id}/deny", user(approvalsHandler.Deny))

	// Unified queue (user JWT)
	queueHandler := handlers.NewQueueHandler(s.store)
	mux.Handle("GET /api/queue", user(queueHandler.List))

	// Overview (user JWT)
	overviewHandler := handlers.NewOverviewHandler(s.store)
	mux.Handle("GET /api/overview", user(overviewHandler.Get))

	// Tasks (agent auth)
	mux.Handle("POST /api/tasks", agent(tasksHandler.Create))
	mux.Handle("GET /api/tasks/{id}", agent(tasksHandler.Get))
	mux.Handle("POST /api/tasks/{id}/complete", agent(tasksHandler.Complete))
	mux.Handle("POST /api/tasks/{id}/expand", agent(tasksHandler.Expand))

	// Tasks (user JWT)
	mux.Handle("GET /api/tasks", user(tasksHandler.List))
	mux.Handle("POST /api/tasks/{id}/approve", user(tasksHandler.Approve))
	mux.Handle("POST /api/tasks/{id}/deny", user(tasksHandler.Deny))
	mux.Handle("POST /api/tasks/{id}/revoke", user(tasksHandler.Revoke))
	mux.Handle("POST /api/tasks/{id}/expand/approve", user(tasksHandler.ExpandApprove))
	mux.Handle("POST /api/tasks/{id}/expand/deny", user(tasksHandler.ExpandDeny))

	// Audit (user JWT)
	mux.Handle("GET /api/audit", user(auditHandler.List))
	mux.Handle("GET /api/audit/{id}", user(auditHandler.Get))

	// Skill files (no auth — served so OpenClaw instances can install the skill)
	// GET /skill         → redirects to /skill/SKILL.md
	// GET /skill/*       → embedded clawvisor skill tree (SKILL.md, policies/, …)
	skillFS, _ := fs.Sub(skillfiles.FS, "clawvisor")
	skillFileHandler := http.StripPrefix("/skill", http.FileServer(http.FS(skillFS)))
	mux.HandleFunc("GET /skill", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/skill/SKILL.md", http.StatusFound)
	})
	mux.Handle("/skill/", skillFileHandler)

	// SPA fallback
	if s.cfg.Server.FrontendDir != "" {
		fs := http.FileServer(http.Dir(s.cfg.Server.FrontendDir))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			path := s.cfg.Server.FrontendDir + r.URL.Path
			if _, err := os.Stat(path); os.IsNotExist(err) || r.URL.Path == "/" {
				http.ServeFile(w, r, s.cfg.Server.FrontendDir+"/index.html")
				return
			}
			fs.ServeHTTP(w, r)
		})
	}

	return securityMiddleware(logMiddleware(mux))
}

// consumeTelegramDecisions reads from the Telegram notifier's decision channel
// and routes approve/deny decisions to the appropriate handler.
func (s *Server) consumeTelegramDecisions(ctx context.Context, ch <-chan notify.CallbackDecision) {
	for {
		select {
		case <-ctx.Done():
			return
		case d, ok := <-ch:
			if !ok {
				return
			}
			var err error
			switch d.Type {
			case "approval":
				if d.Action == "approve" {
					err = s.approvalsHandler.ApproveByRequestID(ctx, d.TargetID, d.UserID)
				} else {
					err = s.approvalsHandler.DenyByRequestID(ctx, d.TargetID, d.UserID)
				}
			case "task":
				if d.Action == "approve" {
					err = s.tasksHandler.ApproveByTaskID(ctx, d.TargetID, d.UserID)
				} else {
					err = s.tasksHandler.DenyByTaskID(ctx, d.TargetID, d.UserID)
				}
			case "scope_expansion":
				if d.Action == "approve" {
					err = s.tasksHandler.ExpandApproveByTaskID(ctx, d.TargetID, d.UserID)
				} else {
					err = s.tasksHandler.ExpandDenyByTaskID(ctx, d.TargetID, d.UserID)
				}
			}
			if err != nil {
				s.logger.Warn("telegram decision failed",
					"type", d.Type, "action", d.Action,
					"target_id", d.TargetID, "err", err)
			}
		}
	}
}

// Handler returns the HTTP handler, primarily for use in tests.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

// Run starts the HTTP server and blocks until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// Start background expiry cleanup.
	go s.approvalsHandler.RunExpiryCleanup(ctx)

	// Start Telegram inline callback consumer and token cleanup.
	if tg, ok := s.notifier.(interface {
		DecisionChannel() <-chan notify.CallbackDecision
		RunCleanup(context.Context)
	}); ok {
		go tg.RunCleanup(ctx)
		go s.consumeTelegramDecisions(ctx, tg.DecisionChannel())
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("server starting", "addr", s.http.Addr)
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("server error: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.logger.Info("shutting down server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		s.store.Close()
		s.logger.Info("server stopped")
		return nil
	}
}

// newKeyedLimiterFromBucket creates a KeyedLimiter from a config bucket.
// Returns nil when the bucket has zero values (unconfigured).
func newKeyedLimiterFromBucket(b config.RateLimitBucket) *ratelimit.KeyedLimiter {
	if b.Limit <= 0 || b.Window <= 0 {
		return nil
	}
	return ratelimit.NewKeyedLimiter(
		rate.Limit(float64(b.Limit)/float64(b.Window)),
		b.Limit,
	)
}
