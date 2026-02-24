package api

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/ericlevine/clawvisor/internal/adapters"
	"github.com/ericlevine/clawvisor/internal/api/handlers"
	"github.com/ericlevine/clawvisor/internal/api/middleware"
	"github.com/ericlevine/clawvisor/internal/auth"
	"github.com/ericlevine/clawvisor/internal/config"
	"github.com/ericlevine/clawvisor/internal/notify"
	"github.com/ericlevine/clawvisor/internal/policy"
	"github.com/ericlevine/clawvisor/internal/policy/authoring"
	"github.com/ericlevine/clawvisor/internal/safety"
	"github.com/ericlevine/clawvisor/internal/store"
	"github.com/ericlevine/clawvisor/internal/vault"
	skillfiles "github.com/ericlevine/clawvisor/skills"
)

// Server is the Clawvisor HTTP server.
type Server struct {
	cfg        *config.Config
	store      store.Store
	vault      vault.Vault
	jwtSvc     *auth.JWTService
	registry   *policy.Registry
	adapterReg *adapters.Registry
	notifier   notify.Notifier
	safety     safety.SafetyChecker
	llmCfg     config.LLMConfig
	logger     *slog.Logger
	http       *http.Server

	// approvalsCleaner is used to stop the background goroutine.
	approvalsHandler *handlers.ApprovalsHandler
}

// New creates a Server and registers all routes.
func New(
	cfg *config.Config,
	st store.Store,
	v vault.Vault,
	jwtSvc *auth.JWTService,
	reg *policy.Registry,
	adapterReg *adapters.Registry,
	notifier notify.Notifier,
	safetyChecker safety.SafetyChecker,
	llmCfg config.LLMConfig,
) (*Server, error) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	s := &Server{
		cfg:        cfg,
		store:      st,
		vault:      v,
		jwtSvc:     jwtSvc,
		registry:   reg,
		adapterReg: adapterReg,
		notifier:   notifier,
		safety:     safetyChecker,
		llmCfg:     llmCfg,
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

	// Normalize bind address to a user-reachable host. 0.0.0.0 / 127.0.0.1
	// are valid bind addresses but not useful as redirect targets in a browser.
	baseHost := s.cfg.Server.Host
	if baseHost == "0.0.0.0" || baseHost == "127.0.0.1" || baseHost == "" {
		baseHost = "localhost"
	}
	baseURL := fmt.Sprintf("http://%s:%d", baseHost, s.cfg.Server.Port)

	// Handlers
	authHandler := handlers.NewAuthHandler(s.jwtSvc, s.store, s.cfg.Auth)
	healthHandler := handlers.NewHealthHandler(s.store, s.vault)
	rolesHandler := handlers.NewRolesHandler(s.store)
	gen := authoring.NewGenerator(s.llmCfg)
	cc := authoring.NewConflictChecker(s.llmCfg)
	policiesHandler := handlers.NewPoliciesHandler(s.store, s.registry, gen, cc)
	agentsHandler := handlers.NewAgentsHandler(s.store)
	auditHandler := handlers.NewAuditHandler(s.store)
	notificationsHandler := handlers.NewNotificationsHandler(s.store)
	gatewayHandler := handlers.NewGatewayHandler(
		s.store, s.vault, s.adapterReg, s.registry,
		s.notifier, s.safety, s.llmCfg, *s.cfg, s.logger, baseURL,
	)
	servicesHandler := handlers.NewServicesHandler(s.store, s.vault, s.adapterReg, s.logger, baseURL)
	skillHandler := handlers.NewSkillHandler(s.store, s.vault, s.adapterReg, s.registry, s.logger)
	approvalsHandler := handlers.NewApprovalsHandler(s.store, s.vault, s.adapterReg, s.logger)
	s.approvalsHandler = approvalsHandler

	// Middleware
	requireUser := middleware.RequireUser(s.jwtSvc, s.store)
	requireAgent := middleware.RequireAgent(s.store)
	logMiddleware := middleware.Logging(s.logger)

	user := func(h http.HandlerFunc) http.Handler { return requireUser(h) }
	agent := func(h http.HandlerFunc) http.Handler { return requireAgent(h) }

	// Health (no auth)
	mux.HandleFunc("GET /health", healthHandler.Health)
	mux.HandleFunc("GET /ready", healthHandler.Ready)

	// Auth (no auth required)
	mux.HandleFunc("POST /api/auth/register", authHandler.Register)
	mux.HandleFunc("POST /api/auth/login", authHandler.Login)
	mux.HandleFunc("POST /api/auth/refresh", authHandler.Refresh)

	// Auth (requires user JWT)
	mux.Handle("POST /api/auth/logout", user(authHandler.Logout))
	mux.Handle("GET /api/me", user(authHandler.Me))
	mux.Handle("PUT /api/me", user(authHandler.UpdateMe))
	mux.Handle("DELETE /api/me", user(authHandler.DeleteMe))

	// Roles
	mux.Handle("GET /api/roles", user(rolesHandler.List))
	mux.Handle("POST /api/roles", user(rolesHandler.Create))
	mux.Handle("PUT /api/roles/{id}", user(rolesHandler.Update))
	mux.Handle("DELETE /api/roles/{id}", user(rolesHandler.Delete))

	// Policies
	mux.Handle("POST /api/policies/validate", user(policiesHandler.Validate))
	mux.Handle("POST /api/policies/evaluate", user(policiesHandler.Evaluate))
	mux.Handle("POST /api/policies/generate", user(policiesHandler.Generate))
	mux.Handle("GET /api/policies", user(policiesHandler.List))
	mux.Handle("POST /api/policies", user(policiesHandler.Create))
	mux.Handle("GET /api/policies/{id}", user(policiesHandler.Get))
	mux.Handle("PUT /api/policies/{id}", user(policiesHandler.Update))
	mux.Handle("DELETE /api/policies/{id}", user(policiesHandler.Delete))

	// Agents (user JWT)
	mux.Handle("GET /api/agents", user(agentsHandler.List))
	mux.Handle("POST /api/agents", user(agentsHandler.Create))
	mux.Handle("PATCH /api/agents/{id}", user(agentsHandler.UpdateRole))
	mux.Handle("DELETE /api/agents/{id}", user(agentsHandler.Delete))

	// Notifications (user JWT)
	mux.Handle("GET /api/notifications", user(notificationsHandler.List))
	mux.Handle("PUT /api/notifications/telegram", user(notificationsHandler.UpsertTelegram))
	mux.Handle("DELETE /api/notifications/telegram", user(notificationsHandler.DeleteTelegram))

	// Gateway (agent token)
	mux.Handle("POST /api/gateway/request", agent(gatewayHandler.HandleRequest))
	mux.Handle("GET /api/gateway/request/{request_id}/status", agent(gatewayHandler.HandleStatus))

	// Services / OAuth (user JWT)
	mux.Handle("GET /api/services", user(servicesHandler.List))
	mux.Handle("GET /api/oauth/url", user(servicesHandler.OAuthGetURL))     // fetch → returns {"url":"..."}
	mux.Handle("GET /api/oauth/start", user(servicesHandler.OAuthStart))    // kept for compat
	mux.HandleFunc("GET /api/oauth/callback", servicesHandler.OAuthCallback) // no auth: browser redirect
	mux.Handle("POST /api/services/{serviceID}/activate", user(servicesHandler.Activate))
	mux.Handle("POST /api/services/{serviceID}/activate-key", user(servicesHandler.ActivateWithKey))

	// Skill catalog (agent token)
	mux.Handle("GET /api/skill/catalog", agent(skillHandler.Catalog))

	// Approvals (user JWT)
	mux.Handle("GET /api/approvals", user(approvalsHandler.List))
	mux.Handle("POST /api/approvals/{request_id}/approve", user(approvalsHandler.Approve))
	mux.Handle("POST /api/approvals/{request_id}/deny", user(approvalsHandler.Deny))

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

	return logMiddleware(mux)
}

// Handler returns the HTTP handler, primarily for use in tests.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

// Run starts the HTTP server and blocks until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// Start background expiry cleanup.
	go s.approvalsHandler.RunExpiryCleanup(ctx)

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
