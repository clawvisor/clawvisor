package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/ericlevine/clawvisor/internal/api/handlers"
	"github.com/ericlevine/clawvisor/internal/api/middleware"
	"github.com/ericlevine/clawvisor/internal/auth"
	"github.com/ericlevine/clawvisor/internal/config"
	"github.com/ericlevine/clawvisor/internal/policy"
	"github.com/ericlevine/clawvisor/internal/store"
	"github.com/ericlevine/clawvisor/internal/vault"
)

// Server is the Clawvisor HTTP server.
type Server struct {
	cfg      *config.Config
	store    store.Store
	vault    vault.Vault
	jwtSvc   *auth.JWTService
	registry *policy.Registry
	logger   *slog.Logger
	http     *http.Server
}

// New creates a Server and registers all routes.
func New(cfg *config.Config, st store.Store, v vault.Vault, jwtSvc *auth.JWTService, reg *policy.Registry) (*Server, error) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	s := &Server{
		cfg:      cfg,
		store:    st,
		vault:    v,
		jwtSvc:   jwtSvc,
		registry: reg,
		logger:   logger,
	}

	mux := s.routes()

	s.http = &http.Server{
		Addr:         cfg.Server.Addr(),
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return s, nil
}

// routes builds the HTTP mux with all registered handlers.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	authHandler := handlers.NewAuthHandler(s.jwtSvc, s.store, s.cfg.Auth)
	healthHandler := handlers.NewHealthHandler(s.store, s.vault)
	rolesHandler := handlers.NewRolesHandler(s.store)
	policiesHandler := handlers.NewPoliciesHandler(s.store, s.registry)

	requireUser := middleware.RequireUser(s.jwtSvc, s.store)
	logMiddleware := middleware.Logging(s.logger)

	auth := func(h http.HandlerFunc) http.Handler { return requireUser(h) }

	// Health (no auth)
	mux.HandleFunc("GET /health", healthHandler.Health)
	mux.HandleFunc("GET /ready", healthHandler.Ready)

	// Auth (no auth required)
	mux.HandleFunc("POST /api/auth/register", authHandler.Register)
	mux.HandleFunc("POST /api/auth/login", authHandler.Login)
	mux.HandleFunc("POST /api/auth/refresh", authHandler.Refresh)

	// Auth (requires user JWT)
	mux.Handle("POST /api/auth/logout", auth(authHandler.Logout))
	mux.Handle("GET /api/me", auth(authHandler.Me))

	// Roles
	mux.Handle("GET /api/roles", auth(rolesHandler.List))
	mux.Handle("POST /api/roles", auth(rolesHandler.Create))
	mux.Handle("PUT /api/roles/{id}", auth(rolesHandler.Update))
	mux.Handle("DELETE /api/roles/{id}", auth(rolesHandler.Delete))

	// Policies — specific routes first, then parameterized
	mux.Handle("POST /api/policies/validate", auth(policiesHandler.Validate))
	mux.Handle("POST /api/policies/evaluate", auth(policiesHandler.Evaluate))
	mux.Handle("GET /api/policies", auth(policiesHandler.List))
	mux.Handle("POST /api/policies", auth(policiesHandler.Create))
	mux.Handle("GET /api/policies/{id}", auth(policiesHandler.Get))
	mux.Handle("PUT /api/policies/{id}", auth(policiesHandler.Update))
	mux.Handle("DELETE /api/policies/{id}", auth(policiesHandler.Delete))

	// SPA fallback: serve frontend for all unmatched routes
	if s.cfg.Server.FrontendDir != "" {
		fs := http.FileServer(http.Dir(s.cfg.Server.FrontendDir))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// Try to serve the file; fall back to index.html for SPA routing
			path := s.cfg.Server.FrontendDir + r.URL.Path
			if _, err := os.Stat(path); os.IsNotExist(err) || r.URL.Path == "/" {
				http.ServeFile(w, r, s.cfg.Server.FrontendDir+"/index.html")
				return
			}
			fs.ServeHTTP(w, r)
		})
	}

	// Wrap everything in logging middleware
	return logMiddleware(mux)
}

// Run starts the HTTP server and blocks until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
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
