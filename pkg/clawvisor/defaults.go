package clawvisor

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/callback"
	intvault "github.com/clawvisor/clawvisor/internal/vault"
	imessageadapter "github.com/clawvisor/clawvisor/internal/adapters/apple/imessage"
	githubadapter "github.com/clawvisor/clawvisor/internal/adapters/github"
	calendaradapter "github.com/clawvisor/clawvisor/internal/adapters/google/calendar"
	contactsadapter "github.com/clawvisor/clawvisor/internal/adapters/google/contacts"
	driveadapter "github.com/clawvisor/clawvisor/internal/adapters/google/drive"
	gmailadapter "github.com/clawvisor/clawvisor/internal/adapters/google/gmail"
	linearadapter "github.com/clawvisor/clawvisor/internal/adapters/linear"
	notionadapter "github.com/clawvisor/clawvisor/internal/adapters/notion"
	slackadapter "github.com/clawvisor/clawvisor/internal/adapters/slack"
	stripeadapter "github.com/clawvisor/clawvisor/internal/adapters/stripe"
	twilioadapter "github.com/clawvisor/clawvisor/internal/adapters/twilio"
	telegramnotify "github.com/clawvisor/clawvisor/internal/notify/telegram"
	pgstore "github.com/clawvisor/clawvisor/internal/store/postgres"
	sqlitestore "github.com/clawvisor/clawvisor/internal/store/sqlite"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// DefaultOptions loads config and builds a fully-wired ServerOptions with the
// standard open-source defaults (SQLite or Postgres, local or GCP vault,
// all enabled adapters, Telegram notifier, magic-link auth for local mode).
//
// Cloud/enterprise builds call this, then selectively override fields:
//
//	opts, _ := clawvisor.DefaultOptions(logger)
//	opts.Store = tenancy.Wrap(opts.Store)
//	opts.Features.PasswordAuth = true
//	clawvisor.Run(opts)
func DefaultOptions(logger *slog.Logger) (*ServerOptions, error) {
	ctx := context.Background()

	// ── Config ─────────────────────────────────────────────────────────────
	cfgPath := "config.yaml"
	if p := os.Getenv("CONFIG_FILE"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if cfg.Auth.JWTSecret == "" {
		if cfg.Server.IsLocal() {
			secret := make([]byte, 32)
			if _, err := cryptorand.Read(secret); err != nil {
				return nil, fmt.Errorf("generating JWT secret: %w", err)
			}
			cfg.Auth.JWTSecret = hex.EncodeToString(secret)
			cfg.AutoConfig.JWTSecret = true
		} else {
			return nil, fmt.Errorf("JWT_SECRET must be set (via env or config.yaml)")
		}
	}

	callback.Init(cfg.Callback.AllowPrivateCIDRs, cfg.Callback.RequireHTTPS, !cfg.Server.IsLocal())

	// ── Database + Store ────────────────────────────────────────────────────
	var (
		st      store.Store
		vaultDB *sql.DB
	)

	switch cfg.Database.Driver {
	case "postgres":
		if cfg.Database.PostgresURL == "" {
			return nil, fmt.Errorf("DATABASE_URL must be set for postgres driver")
		}
		pool, err := pgstore.New(ctx, cfg.Database.PostgresURL)
		if err != nil {
			return nil, fmt.Errorf("connecting to postgres: %w", err)
		}
		st = pgstore.NewStore(pool)
		vaultDB = stdlib.OpenDBFromPool(pool)

	case "sqlite":
		db, err := sqlitestore.New(ctx, cfg.Database.SQLitePath)
		if err != nil {
			return nil, fmt.Errorf("connecting to sqlite: %w", err)
		}
		st = sqlitestore.NewStore(db)
		vaultDB = db

	default:
		return nil, fmt.Errorf("unsupported database driver %q (use \"postgres\" or \"sqlite\")", cfg.Database.Driver)
	}

	// ── Auth ────────────────────────────────────────────────────────────────
	jwtSvc, err := auth.NewJWTService(cfg.Auth.JWTSecret)
	if err != nil {
		return nil, err
	}

	// ── Vault ───────────────────────────────────────────────────────────────
	v, err := buildVault(cfg, vaultDB, cfg.Database.Driver)
	if err != nil {
		return nil, err
	}

	// ── Adapter Registry ─────────────────────────────────────────────────────
	adapterReg := adapters.NewRegistry()
	if cfg.Services.Google.ClientID != "" {
		redirectURL := cfg.Services.Google.RedirectURL
		if redirectURL == "" {
			host := cfg.Server.Host
			if host == "0.0.0.0" || host == "127.0.0.1" || host == "" {
				host = "localhost"
			}
			redirectURL = fmt.Sprintf("http://%s:%d/api/oauth/callback", host, cfg.Server.Port)
		}
		adapterReg.Register(gmailadapter.New(cfg.Services.Google.ClientID, cfg.Services.Google.ClientSecret, redirectURL))
		adapterReg.Register(calendaradapter.New(cfg.Services.Google.ClientID, cfg.Services.Google.ClientSecret, redirectURL))
		adapterReg.Register(driveadapter.New(cfg.Services.Google.ClientID, cfg.Services.Google.ClientSecret, redirectURL))
		adapterReg.Register(contactsadapter.New(cfg.Services.Google.ClientID, cfg.Services.Google.ClientSecret, redirectURL))
	}
	if cfg.Services.GitHub.Enabled {
		adapterReg.Register(githubadapter.New())
	}
	if cfg.Services.Slack.Enabled {
		adapterReg.Register(slackadapter.New())
	}
	if cfg.Services.Notion.Enabled {
		adapterReg.Register(notionadapter.New())
	}
	if cfg.Services.Linear.Enabled {
		adapterReg.Register(linearadapter.New())
	}
	if cfg.Services.Stripe.Enabled {
		adapterReg.Register(stripeadapter.New())
	}
	if cfg.Services.Twilio.Enabled {
		adapterReg.Register(twilioadapter.New())
	}
	if cfg.Services.IMessage.Enabled {
		imsg := imessageadapter.New()
		if imsg.Available() {
			adapterReg.Register(imsg)
		}
	}

	// ── Notifier ─────────────────────────────────────────────────────────────
	var notifier notify.Notifier = telegramnotify.New(st, ctx)

	// ── Auth mode ──────────────────────────────────────────────────────────
	// Default is magic-link. Set auth_mode: "password" in config.yaml
	// (or AUTH_MODE=password env var) to enable email/password auth.
	magicStore := auth.NewMagicTokenStore()

	features := FeatureSet{
		PasswordAuth: cfg.Server.AuthMode == "password",
	}

	return &ServerOptions{
		Logger:     logger,
		Config:     cfg,
		Store:      st,
		Vault:      v,
		JWTService: jwtSvc,
		AdapterReg: adapterReg,
		Notifier:   notifier,
		MagicStore: magicStore,
		Features:   features,
	}, nil
}

// ConnectStore loads config and connects to the database only.
// Use for CLI commands that need DB access without the full server stack
// (no vault, adapters, notifier, or JWT initialization).
func ConnectStore(logger *slog.Logger) (*config.Config, store.Store, error) {
	ctx := context.Background()

	cfgPath := "config.yaml"
	if p := os.Getenv("CONFIG_FILE"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}

	var st store.Store
	switch cfg.Database.Driver {
	case "postgres":
		if cfg.Database.PostgresURL == "" {
			return nil, nil, fmt.Errorf("DATABASE_URL must be set for postgres driver")
		}
		pool, err := pgstore.New(ctx, cfg.Database.PostgresURL)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to postgres: %w", err)
		}
		st = pgstore.NewStore(pool)
	case "sqlite":
		db, err := sqlitestore.New(ctx, cfg.Database.SQLitePath)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to sqlite: %w", err)
		}
		st = sqlitestore.NewStore(db)
	default:
		return nil, nil, fmt.Errorf("unsupported database driver %q (use \"postgres\" or \"sqlite\")", cfg.Database.Driver)
	}

	return cfg, st, nil
}

func buildVault(cfg *config.Config, db *sql.DB, driver string) (vault.Vault, error) {
	switch cfg.Vault.Backend {
	case "local":
		key, err := intvault.ResolveKey(cfg.Vault.MasterKey, cfg.Vault.LocalKeyFile)
		if err != nil {
			return nil, fmt.Errorf("resolving vault key: %w", err)
		}
		return intvault.NewLocalVaultFromKeyWithDB(key, db, driver)
	case "gcp":
		if cfg.Vault.GCPProject == "" {
			return nil, fmt.Errorf("GCP_PROJECT must be set for gcp vault backend")
		}
		key, err := intvault.ResolveKey(cfg.Vault.MasterKey, cfg.Vault.LocalKeyFile)
		if err != nil {
			return nil, fmt.Errorf("resolving vault key for gcp backend: %w", err)
		}
		return intvault.NewGCPVault(context.Background(), cfg.Vault.GCPProject, key)
	default:
		return nil, fmt.Errorf("unsupported vault backend %q (use \"local\" or \"gcp\")", cfg.Vault.Backend)
	}
}
