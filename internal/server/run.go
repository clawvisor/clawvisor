package server

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/browser"
	"github.com/clawvisor/clawvisor/internal/telemetry"
	"github.com/clawvisor/clawvisor/pkg/clawvisor"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// RunOptions holds optional flags for the server entrypoint.
type RunOptions struct {
	OpenBrowser bool
	ConfigPath  string // If set, passed to DefaultOptions instead of using env var.
}

// LocalAuthResult holds the outputs of SetupLocalAuth.
type LocalAuthResult struct {
	MagicURL   string // e.g. "http://localhost:25297/magic-link?token=..."
	MagicToken string // raw one-time token for API client authentication
	ServerURL  string // e.g. "http://localhost:25297"
}

// SetupLocalAuth creates the admin@local user (if not present), generates magic
// tokens, and writes ~/.clawvisor/.local-session. Call this after
// DefaultOptions but before Run/RunWithContext when starting the server without
// going through the standard Run entrypoint.
func SetupLocalAuth(opts *clawvisor.ServerOptions, logger *slog.Logger) (*LocalAuthResult, error) {
	ms := opts.MagicStore
	if ms == nil {
		return &LocalAuthResult{}, nil
	}

	cfg := opts.Config
	const localEmail = "admin@local"
	bgCtx := context.Background()

	_, uErr := opts.Store.GetUserByEmail(bgCtx, localEmail)
	if uErr != nil {
		randPw := make([]byte, 32)
		if _, err := cryptorand.Read(randPw); err != nil {
			return nil, fmt.Errorf("generating random password: %w", err)
		}
		hash, err := auth.HashPassword(hex.EncodeToString(randPw))
		if err != nil {
			return nil, fmt.Errorf("hashing local user password: %w", err)
		}
		if _, err := opts.Store.CreateUser(bgCtx, localEmail, hash); err != nil {
			return nil, fmt.Errorf("creating local user: %w", err)
		}
		logger.Debug("created local user", "email", localEmail)
	}

	localUser, err := opts.Store.GetUserByEmail(bgCtx, localEmail)
	if err != nil {
		return nil, fmt.Errorf("loading local user: %w", err)
	}

	displayHost := cfg.Server.Host
	if displayHost == "0.0.0.0" || displayHost == "127.0.0.1" || displayHost == "" {
		displayHost = "localhost"
	}
	serverURL := fmt.Sprintf("http://%s:%d", displayHost, cfg.Server.Port)

	token, err := ms.Generate(localUser.ID)
	if err != nil {
		return nil, fmt.Errorf("generating magic token: %w", err)
	}
	magicURL := fmt.Sprintf("%s/magic-link?token=%s", serverURL, token)

	// TUI auto-login token (written to .local-session).
	tuiToken, err := ms.Generate(localUser.ID)
	if err != nil {
		logger.Warn("could not generate TUI magic token", "err", err)
	} else {
		writeLocalSession(serverURL, tuiToken, logger)
	}

	// Start cleanup goroutine (runs until process exits).
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			ms.Cleanup()
		}
	}()

	return &LocalAuthResult{
		MagicURL:   magicURL,
		MagicToken: tuiToken,
		ServerURL:  serverURL,
	}, nil
}

// Run starts the Clawvisor API server. This is the main entrypoint called
// from cmd/clawvisor/cmd_server.go.
func Run(logger *slog.Logger, ropts RunOptions) error {
	opts, err := clawvisor.DefaultOptions(logger, ropts.ConfigPath)
	if err != nil {
		return err
	}

	// ── Magic link setup (local mode) ──────────────────────────────────────
	authResult, err := SetupLocalAuth(opts, logger)
	if err != nil {
		return err
	}

	// ── Banner ─────────────────────────────────────────────────────────────
	if opts.Config.Server.IsLocal() {
		printBanner(opts.Config, authResult.MagicURL)
	}

	if opts.PushNotifier != nil {
		logger.Info("push notifier enabled", "push_url", opts.Config.Push.URL, "daemon_id", opts.Config.Relay.DaemonID)
	}

	if ropts.OpenBrowser && authResult.MagicURL != "" {
		browser.Open(authResult.MagicURL)
	}

	// ── Telemetry ──────────────────────────────────────────────────────────
	telemetry.Start(context.Background(), opts.Config, opts.Store, logger)

	return clawvisor.Run(opts)
}

// ── Banner & helpers ────────────────────────────────────────────────────────

func printBanner(cfg *config.Config, magicURL string) {
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
