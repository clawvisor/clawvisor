package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/ericlevine/clawvisor/internal/api"
	"github.com/ericlevine/clawvisor/internal/auth"
	"github.com/ericlevine/clawvisor/internal/config"
	"github.com/ericlevine/clawvisor/internal/policy"
	"github.com/ericlevine/clawvisor/internal/store"
	pgstore "github.com/ericlevine/clawvisor/internal/store/postgres"
	sqlitestore "github.com/ericlevine/clawvisor/internal/store/sqlite"
	"github.com/ericlevine/clawvisor/internal/vault"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ── Config ─────────────────────────────────────────────────────────────
	cfgPath := "config.yaml"
	if p := os.Getenv("CONFIG_FILE"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if cfg.Auth.JWTSecret == "" {
		return fmt.Errorf("JWT_SECRET must be set (via env or config.yaml)")
	}

	// ── Database + Store ────────────────────────────────────────────────────
	var (
		st      store.Store
		vaultDB *sql.DB
	)

	switch cfg.Database.Driver {
	case "postgres":
		if cfg.Database.PostgresURL == "" {
			return fmt.Errorf("DATABASE_URL must be set for postgres driver")
		}
		pool, err := pgstore.New(ctx, cfg.Database.PostgresURL)
		if err != nil {
			return fmt.Errorf("connecting to postgres: %w", err)
		}
		st = pgstore.NewStore(pool)
		vaultDB = stdlib.OpenDBFromPool(pool)

	case "sqlite":
		db, err := sqlitestore.New(ctx, cfg.Database.SQLitePath)
		if err != nil {
			return fmt.Errorf("connecting to sqlite: %w", err)
		}
		sqliteSt := sqlitestore.NewStore(db)
		st = sqliteSt
		vaultDB = db

	default:
		return fmt.Errorf("unsupported database driver %q (use \"postgres\" or \"sqlite\")", cfg.Database.Driver)
	}

	// ── Auth ────────────────────────────────────────────────────────────────
	jwtSvc, err := auth.NewJWTService(cfg.Auth.JWTSecret)
	if err != nil {
		return err
	}

	// ── Vault ───────────────────────────────────────────────────────────────
	v, err := buildVault(cfg, vaultDB, cfg.Database.Driver)
	if err != nil {
		return err
	}

	// ── Policy Registry ─────────────────────────────────────────────────────
	reg := policy.NewRegistry()
	if err := loadPoliciesIntoRegistry(ctx, st, reg, logger); err != nil {
		return fmt.Errorf("loading policies: %w", err)
	}

	// ── HTTP Server ─────────────────────────────────────────────────────────
	srv, err := api.New(cfg, st, v, jwtSvc, reg)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

// loadPoliciesIntoRegistry fetches all users' policies from the DB and compiles
// them into the registry. This runs once at startup.
func loadPoliciesIntoRegistry(ctx context.Context, st store.Store, reg *policy.Registry, logger *slog.Logger) error {
	// We don't have a ListAllUsers method yet; we'll leave the registry empty at startup
	// for now. The registry is updated incrementally via API handlers on every policy write.
	// TODO Phase 4: add a batch preload when user list is available.
	logger.Info("policy registry initialized (policies loaded on first access)")
	return nil
}

func buildVault(cfg *config.Config, db *sql.DB, driver string) (vault.Vault, error) {
	switch cfg.Vault.Backend {
	case "local":
		return vault.NewLocalVault(cfg.Vault.LocalKeyFile, db, driver)
	case "gcp":
		if cfg.Vault.GCPProject == "" {
			return nil, fmt.Errorf("GCP_PROJECT must be set for gcp vault backend")
		}
		localKey, err := vault.LoadKey(cfg.Vault.LocalKeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading vault master key for gcp backend: %w", err)
		}
		return vault.NewGCPVault(context.Background(), cfg.Vault.GCPProject, localKey)
	default:
		return nil, fmt.Errorf("unsupported vault backend %q (use \"local\" or \"gcp\")", cfg.Vault.Backend)
	}
}
