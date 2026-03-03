package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/internal/api/handlers"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	pkgauth "github.com/clawvisor/clawvisor/pkg/auth"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/internal/ratelimit"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
	skillfiles "github.com/clawvisor/clawvisor/skills"

	"golang.org/x/time/rate"
)

// Server is the Clawvisor HTTP server.
type Server struct {
	cfg        *config.Config
	store      store.Store
	vault      vault.Vault
	jwtSvc     pkgauth.TokenService
	adapterReg *adapters.Registry
	notifier   notify.Notifier
	llmCfg     config.LLMConfig
	logger     *slog.Logger
	http       *http.Server

	magicStore pkgauth.MagicTokenStore

	// Extension points for open-core customization.
	extraRoutes func(*http.ServeMux, Dependencies)
	wrapRoutes  func(http.Handler) http.Handler
	features    FeatureSet

	// approvalsCleaner is used to stop the background goroutine.
	approvalsHandler *handlers.ApprovalsHandler
	tasksHandler     *handlers.TasksHandler
}

// Dependencies is passed to ExtraRoutes so extension handlers can access shared services.
type Dependencies struct {
	Store      store.Store
	Vault      vault.Vault
	JWTService pkgauth.TokenService
	AdapterReg *adapters.Registry
	Notifier   notify.Notifier
	Logger     *slog.Logger
	BaseURL    string
}

// FeatureSet tells the frontend (and API consumers) which capabilities are available.
// The open-source build returns all false; the cloud build sets the relevant fields.
type FeatureSet struct {
	MultiTenant       bool `json:"multi_tenant"`
	EmailVerification bool `json:"email_verification"`
	Passkeys          bool `json:"passkeys"`
	SSO               bool `json:"sso"`
	Teams             bool `json:"teams"`
	UsageMetering     bool `json:"usage_metering"`
	PasswordAuth      bool `json:"password_auth"`
}

// ServerOption configures optional behavior on the Server.
type ServerOption func(*Server)

// WithExtraRoutes registers additional HTTP routes (e.g. cloud-only endpoints).
func WithExtraRoutes(fn func(*http.ServeMux, Dependencies)) ServerOption {
	return func(s *Server) { s.extraRoutes = fn }
}

// WithWrapRoutes wraps the entire HTTP handler (e.g. tenant-scoping middleware).
func WithWrapRoutes(fn func(http.Handler) http.Handler) ServerOption {
	return func(s *Server) { s.wrapRoutes = fn }
}

// WithFeatures declares which capabilities the frontend should expose.
func WithFeatures(f FeatureSet) ServerOption {
	return func(s *Server) { s.features = f }
}

// New creates a Server and registers all routes.
// magicStore may be nil when magic link auth is not enabled.
func New(
	cfg *config.Config,
	st store.Store,
	v vault.Vault,
	jwtSvc pkgauth.TokenService,
	adapterReg *adapters.Registry,
	notifier notify.Notifier,
	llmCfg config.LLMConfig,
	magicStore pkgauth.MagicTokenStore,
	opts ...ServerOption,
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

	// Apply optional configuration.
	for _, o := range opts {
		o(s)
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
	authMode := "magic_link"
	if s.features.PasswordAuth {
		authMode = "password"
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

	// Auth — core routes (always registered)
	mux.HandleFunc("POST /api/auth/refresh", authHandler.Refresh)
	mux.Handle("POST /api/auth/logout", user(authHandler.Logout))
	mux.Handle("GET /api/me", user(authHandler.Me))

	// Magic link auth (local mode only)
	if s.magicStore != nil {
		mux.HandleFunc("POST /api/auth/magic", authHandler.ExchangeMagic)
	}

	// Password auth routes are registered only when the PasswordAuth feature is enabled.
	// In the open-source build this is off by default (local mode uses magic links).
	// Cloud and self-hosted password deployments enable it via WithFeatures.
	if s.features.PasswordAuth {
		mux.HandleFunc("POST /api/auth/register", authHandler.Register)
		mux.HandleFunc("POST /api/auth/login", authHandler.Login)
		mux.Handle("PUT /api/me", user(authHandler.UpdateMe))
		mux.Handle("DELETE /api/me", user(authHandler.DeleteMe))
	}

	// Features endpoint (always registered, returns the active FeatureSet)
	mux.HandleFunc("GET /api/features", s.handleFeatures)

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

	// Callback secret registration (agent token)
	mux.Handle("POST /api/callbacks/register", agent(gatewayHandler.RegisterCallback))

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

	// Extension hook: let cloud/enterprise layers add additional routes.
	if s.extraRoutes != nil {
		s.extraRoutes(mux, Dependencies{
			Store:      s.store,
			Vault:      s.vault,
			JWTService: s.jwtSvc,
			AdapterReg: s.adapterReg,
			Notifier:   s.notifier,
			Logger:     s.logger,
			BaseURL:    baseURL,
		})
	}

	// SPA fallback
	if s.cfg.Server.FrontendDir != "" {
		fs := http.FileServer(http.Dir(s.cfg.Server.FrontendDir))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			path := s.cfg.Server.FrontendDir + r.URL.Path
			if _, err := os.Stat(path); os.IsNotExist(err) || r.URL.Path == "/" {
				// index.html must not be cached — it references content-hashed
				// JS/CSS chunks, so a stale copy serves outdated code.
				w.Header().Set("Cache-Control", "no-cache")
				http.ServeFile(w, r, s.cfg.Server.FrontendDir+"/index.html")
				return
			}
			fs.ServeHTTP(w, r)
		})
	}

	handler := securityMiddleware(logMiddleware(mux))

	// Extension hook: let cloud/enterprise layers wrap the entire handler.
	if s.wrapRoutes != nil {
		handler = s.wrapRoutes(handler)
	}

	return handler
}

// handleFeatures returns the active feature set as JSON.
func (s *Server) handleFeatures(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.features)
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
		if !s.cfg.Server.IsLocal() {
			s.logger.Info("server starting", "addr", s.http.Addr)
		}
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("server error: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		if s.cfg.Server.IsLocal() {
			fmt.Println("\n  Shutting down...")
		} else {
			s.logger.Info("shutting down server")
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		s.store.Close()
		if !s.cfg.Server.IsLocal() {
			s.logger.Info("server stopped")
		}
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
