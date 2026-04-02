package clawvisor

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/clawvisor/clawvisor/internal/adapters/definitions"
	imessageadapter "github.com/clawvisor/clawvisor/internal/adapters/apple/imessage"
	sqladapter "github.com/clawvisor/clawvisor/internal/adapters/sql"
	driveadapter "github.com/clawvisor/clawvisor/internal/adapters/google/drive"
	gmailadapter "github.com/clawvisor/clawvisor/internal/adapters/google/gmail"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/callback"
	"github.com/clawvisor/clawvisor/internal/display"
	"github.com/clawvisor/clawvisor/internal/relay"
	intvault "github.com/clawvisor/clawvisor/internal/vault"
	pushnotify "github.com/clawvisor/clawvisor/internal/notify/push"
	telegramnotify "github.com/clawvisor/clawvisor/internal/notify/telegram"
	pgstore "github.com/clawvisor/clawvisor/internal/store/postgres"
	sqlitestore "github.com/clawvisor/clawvisor/internal/store/sqlite"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlloader"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlruntime"
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
// DefaultOptions builds a ServerOptions from config. If configPath is provided,
// it is used directly; otherwise CONFIG_FILE env var is consulted, falling back
// to "config.yaml".
func DefaultOptions(logger *slog.Logger, configPath ...string) (*ServerOptions, error) {
	ctx := context.Background()

	// ── Config ─────────────────────────────────────────────────────────────
	cfgPath := "config.yaml"
	if len(configPath) > 0 && configPath[0] != "" {
		cfgPath = configPath[0]
	} else if p := os.Getenv("CONFIG_FILE"); p != "" {
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

	// Create a vault-backed OAuth provider for Google services.
	// Reads credentials lazily — supports adding OAuth creds without restart.
	oauthProvider := adapters.NewVaultOAuthProvider(v)

	// Build Go action overrides for Google services that need complex logic
	// (MIME encoding, multipart uploads, dual API calls) beyond what YAML
	// expressions can handle. Calendar, Contacts, Drive search_files, and
	// Gmail get_message are now fully YAML-driven with expr-lang transforms.
	goOverrides := map[string]yamlruntime.ActionFunc{}

	gmail := gmailadapter.New(oauthProvider)
	for _, action := range []string{"list_messages", "send_message", "create_draft"} {
		goOverrides["google.gmail:"+action] = gmail.Execute
	}
	drive := driveadapter.New(oauthProvider)
	for _, action := range []string{"get_file", "create_file", "update_file"} {
		goOverrides["google.drive:"+action] = drive.Execute
	}

	// Load YAML adapter definitions from embedded FS + user-local directory.
	home, _ := os.UserHomeDir()
	userAdaptersDir := filepath.Join(home, ".clawvisor", "adapters")
	yamlLoader := yamlloader.New(definitions.FS, userAdaptersDir, goOverrides, logger)
	if err := yamlLoader.LoadAll(); err != nil {
		return nil, fmt.Errorf("loading adapter definitions: %w", err)
	}

	// Register all YAML adapters. No enabled/disabled check — presence of the
	// YAML definition means the service is available. OAuth services that lack
	// credentials will show as "needs_setup" in the UI.
	for _, ya := range yamlLoader.Adapters() {
		meta := ya.ServiceMetadata()
		if meta.OAuthEndpoint == "google" {
			ya.SetOAuthProvider(oauthProvider)
		}
		adapterReg.Register(ya)
	}

	// Go-only adapters (not YAML-driven).
	adapterReg.Register(imessageadapter.New())
	adapterReg.Register(sqladapter.New())

	// Initialize the display package with the adapter registry.
	display.Init(adapterReg)

	// ── Ed25519 key (shared by push + relay) ─────────────────────────────────
	var ed25519Key ed25519.PrivateKey
	var dataDir string
	if cfg.Relay.DaemonID != "" {
		dataDir = cfg.Daemon.DataDir
		if dataDir == "~/.clawvisor" {
			home, _ := os.UserHomeDir()
			dataDir = filepath.Join(home, ".clawvisor")
		}
		keyPath := cfg.Relay.KeyFile
		if !filepath.IsAbs(keyPath) {
			keyPath = filepath.Join(dataDir, keyPath)
		}
		key, err := loadEd25519KeyFile(keyPath)
		if err != nil {
			logger.Warn("could not load Ed25519 key, relay+push disabled", "err", err)
		} else {
			ed25519Key = key
		}
	}

	// ── Notifier ─────────────────────────────────────────────────────────────
	telegramN := telegramnotify.New(st, ctx)

	var pushN *pushnotify.Notifier
	if cfg.Push.Enabled && ed25519Key != nil {
		daemonURL := cfg.Server.PublicURL
		if daemonURL == "" && cfg.Relay.Enabled && cfg.Relay.URL != "" && cfg.Relay.DaemonID != "" {
			// Use the relay URL so the phone can route actions back through the relay.
			relayHost := strings.TrimPrefix(strings.TrimPrefix(cfg.Relay.URL, "wss://"), "ws://")
			daemonURL = fmt.Sprintf("https://%s/d/%s", relayHost, cfg.Relay.DaemonID)
		}
		if daemonURL == "" {
			daemonURL = fmt.Sprintf("http://%s:%d", cfg.Server.Host, cfg.Server.Port)
		}
		pushN = pushnotify.New(st, cfg.Push.URL, cfg.Relay.DaemonID, ed25519Key, daemonURL, logger)

		// Register daemon's public key with the push service (idempotent).
		if err := pushN.RegisterDaemon(ctx); err != nil {
			logger.Warn("failed to register daemon with push service", "err", err)
		}
	}

	var notifiers []notify.Notifier
	notifiers = append(notifiers, telegramN)
	if pushN != nil {
		notifiers = append(notifiers, pushN)
	}
	var notifier notify.Notifier = notify.NewMultiNotifier(ctx, logger, notifiers...)

	// ── Auth mode ──────────────────────────────────────────────────────────
	// Default is magic-link. Set auth_mode: "password" in config.yaml
	// (or AUTH_MODE=password env var) to enable email/password auth.
	magicStore := auth.NewMagicTokenStore()

	features := FeatureSet{
		PasswordAuth: cfg.Server.AuthMode == "password",
	}

	opts := &ServerOptions{
		Logger:       logger,
		Config:       cfg,
		Store:        st,
		Vault:        v,
		JWTService:   jwtSvc,
		AdapterReg:   adapterReg,
		Notifier:     notifier,
		PushNotifier: pushN,
		MagicStore:   magicStore,
		Features:     features,
	}

	// Wire relay client when configured.
	if cfg.Relay.Enabled && ed25519Key != nil {
		// Load X25519 key for E2E encryption.
		e2eKeyPath := cfg.Relay.E2EKeyFile
		if !filepath.IsAbs(e2eKeyPath) {
			e2eKeyPath = filepath.Join(dataDir, e2eKeyPath)
		}
		x25519Key, err := loadX25519KeyFile(e2eKeyPath)
		if err != nil {
			logger.Warn("relay: could not load X25519 key, E2E disabled", "err", err)
		} else {
			opts.X25519Key = x25519Key
		}

		relayClient := relay.New(cfg.Relay, ed25519Key, nil, logger)
		opts.RelayClient = relayClient
	} else if cfg.Relay.DaemonID != "" {
		// Even if relay is disabled, load X25519 key for E2E support on local requests.
		e2eKeyPath := cfg.Relay.E2EKeyFile
		if !filepath.IsAbs(e2eKeyPath) {
			e2eKeyPath = filepath.Join(dataDir, e2eKeyPath)
		}
		x25519Key, err := loadX25519KeyFile(e2eKeyPath)
		if err == nil {
			opts.X25519Key = x25519Key
		}
	}

	return opts, nil
}

// loadEd25519KeyFile reads a PEM-encoded Ed25519 private key seed.
func loadEd25519KeyFile(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", path)
	}
	return ed25519.NewKeyFromSeed(block.Bytes), nil
}

// loadX25519KeyFile reads a raw 32-byte X25519 private key and returns
// an *ecdh.PrivateKey.
func loadX25519KeyFile(path string) (*ecdh.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) != 32 {
		return nil, fmt.Errorf("X25519 key must be 32 bytes, got %d", len(data))
	}
	return ecdh.X25519().NewPrivateKey(data)
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
