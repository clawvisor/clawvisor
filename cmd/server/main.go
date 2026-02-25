package main

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/ericlevine/clawvisor/internal/adapters"
	imessageadapter "github.com/ericlevine/clawvisor/internal/adapters/apple/imessage"
	githubadapter "github.com/ericlevine/clawvisor/internal/adapters/github"
	calendaradapter "github.com/ericlevine/clawvisor/internal/adapters/google/calendar"
	contactsadapter "github.com/ericlevine/clawvisor/internal/adapters/google/contacts"
	driveadapter "github.com/ericlevine/clawvisor/internal/adapters/google/drive"
	gmailadapter "github.com/ericlevine/clawvisor/internal/adapters/google/gmail"
	"github.com/ericlevine/clawvisor/internal/api"
	"github.com/ericlevine/clawvisor/internal/auth"
	"github.com/ericlevine/clawvisor/internal/config"
	"github.com/ericlevine/clawvisor/internal/notify"
	telegramnotify "github.com/ericlevine/clawvisor/internal/notify/telegram"
	"github.com/ericlevine/clawvisor/internal/safety"
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

	// ── Adapter Registry ─────────────────────────────────────────────────────
	adapterReg := adapters.NewRegistry()
	if cfg.Google.ClientID != "" {
		redirectURL := cfg.Google.RedirectURL
		if redirectURL == "" {
			// Normalize the host for the OAuth redirect URL. When the server binds to
			// 0.0.0.0 or 127.0.0.1, use "localhost" instead — Google Cloud Console
			// requires the redirect URI to match exactly and users typically register
			// http://localhost:PORT, not http://0.0.0.0:PORT or http://127.0.0.1:PORT.
			host := cfg.Server.Host
			if host == "0.0.0.0" || host == "127.0.0.1" || host == "" {
				host = "localhost"
			}
			redirectURL = fmt.Sprintf("http://%s:%d/api/oauth/callback", host, cfg.Server.Port)
		}
		// Register all Google adapters — they share the same OAuth credentials (vault key "google").
		adapterReg.Register(gmailadapter.New(cfg.Google.ClientID, cfg.Google.ClientSecret, redirectURL))
		adapterReg.Register(calendaradapter.New(cfg.Google.ClientID, cfg.Google.ClientSecret, redirectURL))
		adapterReg.Register(driveadapter.New(cfg.Google.ClientID, cfg.Google.ClientSecret, redirectURL))
		adapterReg.Register(contactsadapter.New(cfg.Google.ClientID, cfg.Google.ClientSecret, redirectURL))
		logger.Info("google adapters registered (gmail, calendar, drive, contacts)")
	} else {
		logger.Info("google adapters not registered (GOOGLE_CLIENT_ID not set)")
	}

	// GitHub adapter — always registered; activated per-user via API key.
	adapterReg.Register(githubadapter.New())
	logger.Info("github adapter registered")

	// iMessage adapter — only registered if available (macOS with chat.db).
	imsg := imessageadapter.New()
	if imsg.Available() {
		adapterReg.Register(imsg)
		logger.Info("imessage adapter registered")
	} else {
		logger.Info("imessage adapter not available (requires macOS with Messages.app configured)")
	}

	// ── Notifier ─────────────────────────────────────────────────────────────
	// Per-user bot tokens are stored in notification_configs. The notifier
	// is always created; it reads credentials from the store on each call.
	var notifier notify.Notifier = telegramnotify.New(st, ctx)
	logger.Info("telegram notifier enabled (per-user bot tokens)")

	// ── Safety Checker ───────────────────────────────────────────────────────
	var safetyChecker safety.SafetyChecker = safety.NoopChecker{}
	if cfg.LLM.Safety.Enabled {
		safetyChecker = safety.NewLLMSafetyChecker(cfg.LLM)
		logger.Info("LLM safety checker enabled", "model", cfg.LLM.Safety.Model)
	}

	// ── Magic link auth (local mode) ────────────────────────────────────────
	var magicStore *auth.MagicTokenStore
	if cfg.Server.IsLocal() {
		magicStore = auth.NewMagicTokenStore()

		// Ensure a default local user exists.
		const localEmail = "admin@local"
		_, err := st.GetUserByEmail(ctx, localEmail)
		if err != nil {
			// User doesn't exist — create one with a random password (never displayed).
			randPw := make([]byte, 32)
			if _, err := cryptorand.Read(randPw); err != nil {
				return fmt.Errorf("generating random password: %w", err)
			}
			hash, err := auth.HashPassword(hex.EncodeToString(randPw))
			if err != nil {
				return fmt.Errorf("hashing local user password: %w", err)
			}
			if _, err := st.CreateUser(ctx, localEmail, hash); err != nil {
				return fmt.Errorf("creating local user: %w", err)
			}
			logger.Info("created local user", "email", localEmail)
		}

		// Look up the user to get their ID.
		localUser, err := st.GetUserByEmail(ctx, localEmail)
		if err != nil {
			return fmt.Errorf("loading local user: %w", err)
		}

		token, err := magicStore.Generate(localUser.ID)
		if err != nil {
			return fmt.Errorf("generating magic token: %w", err)
		}

		// Normalize the host for the URL shown to the user.
		displayHost := cfg.Server.Host
		if displayHost == "0.0.0.0" || displayHost == "127.0.0.1" || displayHost == "" {
			displayHost = "localhost"
		}
		magicURL := fmt.Sprintf("http://%s:%d/auth/local?token=%s", displayHost, cfg.Server.Port, token)

		fmt.Println()
		fmt.Println("  Clawvisor dashboard")
		fmt.Printf("  %s\n", magicURL)
		fmt.Println()
		fmt.Println("  Open this link in your browser to sign in.")
		fmt.Println("  Valid for 15 minutes. Single use.")
		fmt.Println()

		// Background cleanup goroutine.
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					magicStore.Cleanup()
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// ── HTTP Server ─────────────────────────────────────────────────────────
	srv, err := api.New(cfg, st, v, jwtSvc, adapterReg, notifier, safetyChecker, cfg.LLM, magicStore)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
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
