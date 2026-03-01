package server

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/clawvisor/clawvisor/internal/adapters"
	imessageadapter "github.com/clawvisor/clawvisor/internal/adapters/apple/imessage"
	githubadapter "github.com/clawvisor/clawvisor/internal/adapters/github"
	linearadapter "github.com/clawvisor/clawvisor/internal/adapters/linear"
	notionadapter "github.com/clawvisor/clawvisor/internal/adapters/notion"
	slackadapter "github.com/clawvisor/clawvisor/internal/adapters/slack"
	stripeadapter "github.com/clawvisor/clawvisor/internal/adapters/stripe"
	twilioadapter "github.com/clawvisor/clawvisor/internal/adapters/twilio"
	calendaradapter "github.com/clawvisor/clawvisor/internal/adapters/google/calendar"
	contactsadapter "github.com/clawvisor/clawvisor/internal/adapters/google/contacts"
	driveadapter "github.com/clawvisor/clawvisor/internal/adapters/google/drive"
	gmailadapter "github.com/clawvisor/clawvisor/internal/adapters/google/gmail"
	"github.com/clawvisor/clawvisor/internal/api"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/callback"
	"github.com/clawvisor/clawvisor/internal/config"
	"github.com/clawvisor/clawvisor/internal/notify"
	telegramnotify "github.com/clawvisor/clawvisor/internal/notify/telegram"
	"github.com/clawvisor/clawvisor/internal/store"
	pgstore "github.com/clawvisor/clawvisor/internal/store/postgres"
	sqlitestore "github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/internal/vault"
)

// Run starts the Clawvisor API server. This is the main entrypoint extracted
// from cmd/server/main.go so it can be called from the unified binary.
func Run(logger *slog.Logger) error {
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
	// Auto-generate JWT secret for local dev; fail fast in non-local mode.
	if cfg.Auth.JWTSecret == "" {
		if cfg.Server.IsLocal() {
			secret := make([]byte, 32)
			if _, err := cryptorand.Read(secret); err != nil {
				return fmt.Errorf("generating JWT secret: %w", err)
			}
			cfg.Auth.JWTSecret = hex.EncodeToString(secret)
			cfg.AutoConfig.JWTSecret = true
		} else {
			return fmt.Errorf("JWT_SECRET must be set (via env or config.yaml)")
		}
	}

	// Initialize callback CIDR allowlist and HTTPS enforcement (before any gateway requests).
	callback.Init(cfg.Callback.AllowPrivateCIDRs, cfg.Callback.RequireHTTPS)

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
	adStatus := adapterStatus{skipped: make(map[string]string)}

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
		adStatus.registered = append(adStatus.registered, "Gmail", "Calendar", "Drive", "Contacts")
	} else {
		adStatus.skipped["Google"] = "set GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET to enable Gmail, Calendar, Drive, and Contacts"
	}

	if cfg.Services.GitHub.Enabled {
		adapterReg.Register(githubadapter.New())
		adStatus.registered = append(adStatus.registered, "GitHub")
	} else {
		adStatus.skipped["GitHub"] = "disabled via config"
	}
	if cfg.Services.Slack.Enabled {
		adapterReg.Register(slackadapter.New())
		adStatus.registered = append(adStatus.registered, "Slack")
	} else {
		adStatus.skipped["Slack"] = "disabled via config"
	}
	if cfg.Services.Notion.Enabled {
		adapterReg.Register(notionadapter.New())
		adStatus.registered = append(adStatus.registered, "Notion")
	} else {
		adStatus.skipped["Notion"] = "disabled via config"
	}
	if cfg.Services.Linear.Enabled {
		adapterReg.Register(linearadapter.New())
		adStatus.registered = append(adStatus.registered, "Linear")
	} else {
		adStatus.skipped["Linear"] = "disabled via config"
	}
	if cfg.Services.Stripe.Enabled {
		adapterReg.Register(stripeadapter.New())
		adStatus.registered = append(adStatus.registered, "Stripe")
	} else {
		adStatus.skipped["Stripe"] = "disabled via config"
	}
	if cfg.Services.Twilio.Enabled {
		adapterReg.Register(twilioadapter.New())
		adStatus.registered = append(adStatus.registered, "Twilio")
	} else {
		adStatus.skipped["Twilio"] = "disabled via config"
	}
	if cfg.Services.IMessage.Enabled {
		imsg := imessageadapter.New()
		if imsg.Available() {
			adapterReg.Register(imsg)
			adStatus.registered = append(adStatus.registered, "iMessage")
		} else {
			adStatus.skipped["iMessage"] = "requires macOS with Messages.app configured"
		}
	} else {
		adStatus.skipped["iMessage"] = "disabled via config"
	}
	logger.Debug("adapters registered", "count", len(adStatus.registered), "names", adStatus.registered)

	// ── Notifier ─────────────────────────────────────────────────────────────
	var notifier notify.Notifier = telegramnotify.New(st, ctx)
	logger.Debug("telegram notifier enabled (per-user bot tokens)")

	// ── Magic link auth (local mode) ────────────────────────────────────────
	var magicStore *auth.MagicTokenStore
	var magicURL string
	useMagicLink := cfg.Server.IsLocal()
	if cfg.Server.AuthMode == "password" {
		useMagicLink = false
	} else if cfg.Server.AuthMode == "magic_link" {
		useMagicLink = true
	}
	if useMagicLink {
		magicStore = auth.NewMagicTokenStore()

		const localEmail = "admin@local"
		_, err := st.GetUserByEmail(ctx, localEmail)
		if err != nil {
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
			logger.Debug("created local user", "email", localEmail)
		}

		localUser, err := st.GetUserByEmail(ctx, localEmail)
		if err != nil {
			return fmt.Errorf("loading local user: %w", err)
		}

		token, err := magicStore.Generate(localUser.ID)
		if err != nil {
			return fmt.Errorf("generating magic token: %w", err)
		}

		displayHost := cfg.Server.Host
		if displayHost == "0.0.0.0" || displayHost == "127.0.0.1" || displayHost == "" {
			displayHost = "localhost"
		}
		magicURL = fmt.Sprintf("http://%s:%d/magic-link?token=%s", displayHost, cfg.Server.Port, token)

		// Generate a second magic token for TUI auto-login and write it to
		// ~/.clawvisor/.local-session so `clawvisor tui` can authenticate
		// without manual token configuration.
		tuiToken, err := magicStore.Generate(localUser.ID)
		if err != nil {
			logger.Warn("could not generate TUI magic token", "err", err)
		} else {
			writeLocalSession(fmt.Sprintf("http://%s:%d", displayHost, cfg.Server.Port), tuiToken, logger)
		}

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

	// ── Banner (local mode only) ─────────────────────────────────────────────
	if cfg.Server.IsLocal() {
		printBanner(cfg, adStatus, magicURL)
	}

	// ── HTTP Server ─────────────────────────────────────────────────────────
	srv, err := api.New(cfg, st, v, jwtSvc, adapterReg, notifier, cfg.LLM, magicStore)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

// ── Banner & helpers ────────────────────────────────────────────────────────

type adapterStatus struct {
	registered []string
	skipped    map[string]string
}

func printBanner(cfg *config.Config, adStatus adapterStatus, magicURL string) {
	const banner = `
   ___  _                       _
  / __\| | __ ___      ____   _(_) ___  ___   _ __
 / /   | |/ _` + "`" + ` \ \ /\ / /\ \ / / |/ __|/ _ \ | '__|
/ /___ | | (_| |\ V  V /  \ V /| |\__ \ (_) || |
\____/ |_|\__,_| \_/\_/    \_/ |_||___/\___/ |_|
`
	fmt.Print(banner)
	fmt.Println("  Clawvisor — AI Gatekeeper")
	fmt.Println()

	fmt.Println("  Configuration")
	fmt.Println("  ─────────────────────────────────────────")

	dbDesc := "Postgres"
	if cfg.Database.Driver == "sqlite" {
		dbDesc = fmt.Sprintf("SQLite (%s)", cfg.Database.SQLitePath)
	}
	if cfg.AutoConfig.DatabaseDriver {
		fmt.Printf("  %-12s %s  (auto)\n", "Database", dbDesc)
	} else {
		fmt.Printf("  %-12s %s\n", "Database", dbDesc)
	}

	vaultDesc := "Local AES-256-GCM"
	if cfg.Vault.Backend == "gcp" {
		vaultDesc = "GCP Secret Manager"
	}
	fmt.Printf("  %-12s %s\n", "Vault", vaultDesc)

	if cfg.AutoConfig.JWTSecret {
		fmt.Printf("  %-12s Generated for this session  (auto)\n", "JWT secret")
	} else {
		fmt.Printf("  %-12s Set via env/config\n", "JWT secret")
	}

	authMode := "Password"
	if magicURL != "" {
		authMode = "Magic link"
	}
	fmt.Printf("  %-12s %s\n", "Auth mode", authMode)
	fmt.Println()

	if len(adStatus.registered) > 0 {
		fmt.Printf("  Adapters (%d registered)\n", len(adStatus.registered))
		fmt.Println("  ─────────────────────────────────────────")
		fmt.Printf("  %s\n", wrapNames(adStatus.registered, 40))
	} else {
		fmt.Println("  Adapters (none registered)")
		fmt.Println("  ─────────────────────────────────────────")
	}

	for name, reason := range adStatus.skipped {
		fmt.Println()
		fmt.Printf("  %s adapters not registered — %s.\n", name, reason)
	}
	fmt.Println()

	fmt.Println("  Dashboard")
	fmt.Println("  ─────────────────────────────────────────")
	if magicURL != "" {
		fmt.Printf("  %s\n", magicURL)
		fmt.Println()
		fmt.Println("  Open this link in your browser to sign in.")
	} else {
		displayHost := cfg.Server.Host
		if displayHost == "0.0.0.0" || displayHost == "127.0.0.1" || displayHost == "" {
			displayHost = "localhost"
		}
		fmt.Printf("  http://%s:%d\n", displayHost, cfg.Server.Port)
	}
	fmt.Println("  Press Ctrl+C to stop the server.")
	fmt.Println()
}

func wrapNames(names []string, maxWidth int) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	lineLen := 0
	for i, n := range names {
		seg := n
		if i < len(names)-1 {
			seg += ","
		}
		needed := len(seg)
		if lineLen > 0 {
			needed++
		}
		if lineLen > 0 && lineLen+needed > maxWidth {
			b.WriteString("\n  ")
			lineLen = 0
		}
		if lineLen > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(seg)
		lineLen += needed
	}
	return b.String()
}


// localSession is the JSON structure written to ~/.clawvisor/.local-session.
type localSession struct {
	ServerURL  string `json:"server_url"`
	MagicToken string `json:"magic_token"`
}

// writeLocalSession writes a .local-session file so the TUI can auto-login.
func writeLocalSession(serverURL, token string, logger *slog.Logger) {
	home, err := os.UserHomeDir()
	if err != nil {
		logger.Warn("could not determine home dir for local session", "err", err)
		return
	}
	dir := filepath.Join(home, ".clawvisor")
	if err := os.MkdirAll(dir, 0700); err != nil {
		logger.Warn("could not create .clawvisor dir", "err", err)
		return
	}
	data, _ := json.Marshal(localSession{
		ServerURL:  serverURL,
		MagicToken: token,
	})
	path := filepath.Join(dir, ".local-session")
	if err := os.WriteFile(path, data, 0600); err != nil {
		logger.Warn("could not write local session file", "err", err)
	}
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
