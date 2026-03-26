package setup

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	tuiconfig "github.com/clawvisor/clawvisor/internal/tui/config"
	"github.com/clawvisor/clawvisor/pkg/haikuproxy"
)

var (
	bold    = lipgloss.NewStyle().Bold(true)
	dim     = lipgloss.NewStyle().Faint(true)
	green   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellow  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	banner  = lipgloss.NewStyle().Bold(true).Padding(0, 2)
	section = lipgloss.NewStyle().Faint(true).Padding(0, 2)
	warnBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("3")).
		Padding(1, 2).
		Margin(0, 2)
)

type config struct {
	envMode   string
	dbDriver  string
	dbURL     string
	jwtSecret string
	vault     string
	gcpProject string
	host      string
	port      string

	googleEnabled bool
	googleID      string
	googleSecret  string

	imessageEnabled bool

	llmProvider string
	llmEndpoint string
	llmModel    string
	llmAPIKey   string

	taskRiskEnabled     bool
	chainContextEnabled bool

	telemetryEnabled bool

	outputDir string // set by RunWithOptions; empty = CWD
}

// RunOptions controls where setup writes its output files.
type RunOptions struct {
	// Dir is the target directory for config.yaml, vault.key, and clawvisor.db.
	// When empty, files are written to the current working directory.
	Dir string
}

// Run executes the setup wizard, writing files to the current directory.
func Run() error {
	return RunWithOptions(RunOptions{})
}

// RunWithOptions executes the setup wizard, writing files to opts.Dir.
func RunWithOptions(opts RunOptions) error {
	printBanner()

	cfg, err := runSetup()
	if err != nil {
		if err == huh.ErrUserAborted {
			fmt.Println("\n  Aborted. No files were written.")
			return nil
		}
		return err
	}

	dir := opts.Dir
	if dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("creating data dir %s: %w", dir, err)
		}
	}

	configPath := filepath.Join(dir, "config.yaml")
	cfg.outputDir = dir

	if err := writeConfig(cfg, configPath); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	// Generate vault key file for local backend (no-op if it already exists).
	if cfg.vault == "local" {
		vaultKeyPath := filepath.Join(dir, "vault.key")
		if err := ensureVaultKey(vaultKeyPath); err != nil {
			return fmt.Errorf("generating vault key: %w", err)
		}
	}

	// Write TUI config so `clawvisor tui` knows the server URL.
	if err := writeTUIConfig(cfg); err != nil {
		// Non-fatal — just warn.
		fmt.Printf("  Warning: could not write TUI config: %v\n", err)
	}

	printNextSteps(cfg)
	return nil
}

func printBanner() {
	art := `
   ___  _                       _
  / __\| | __ ___      ____   _(_) ___  ___   _ __
 / /   | |/ _` + "`" + ` \ \ /\ / /\ \ / / |/ __|/ _ \ | '__|
/ /___ | | (_| |\ V  V /  \ V /| |\__ \ (_) || |
\____/ |_|\__,_| \_/\_/    \_/ |_||___/\___/ |_|`

	fmt.Println(banner.Render(art))
	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Clawvisor Setup"))
	fmt.Println(section.Render("─────────────────────────────────────────"))
	fmt.Println()
	fmt.Println(warnBox.Render(
		yellow.Bold(true).Render("⚠  USE AT YOUR OWN RISK") + "\n\n" +
			"Clawvisor is experimental software under active development.\n" +
			"It has not been audited for security. LLMs are inherently\n" +
			"nondeterministic — we make no guarantees that policies or\n" +
			"safety checks will behave as expected in every case. Do not\n" +
			"use Clawvisor as your sole safeguard for sensitive data or\n" +
			"critical systems."))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("This will generate a config.yaml for your Clawvisor instance."))
	fmt.Println()
}

func runSetup() (*config, error) {
	cfg := &config{
		envMode:  "local",
		dbDriver: "sqlite",
		host:     "127.0.0.1",
		port:     "25297",
		vault:    "local",
	}

	if err := stepEnvironment(cfg); err != nil {
		return nil, err
	}
	if err := stepGoogle(cfg); err != nil {
		return nil, err
	}
	if err := stepIMessage(cfg); err != nil {
		return nil, err
	}
	if err := stepLLM(cfg); err != nil {
		return nil, err
	}
	if err := stepTelemetry(cfg); err != nil {
		return nil, err
	}
	if err := stepConfirm(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func stepEnvironment(cfg *config) error {
	var env string
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Where will Clawvisor run?").
				Options(
					huh.NewOption("Local machine", "local"),
					huh.NewOption("Production server", "production"),
				).
				Value(&env),
		),
	).Run()
	if err != nil {
		return err
	}

	if env == "local" {
		return nil
	}

	cfg.envMode = "production"
	cfg.dbDriver = "postgres"
	cfg.host = "0.0.0.0"

	var jwtChoice string
	err = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("DATABASE_URL").
				Description("Postgres connection string").
				Placeholder("postgres://user:pass@host:5432/clawvisor").
				Value(&cfg.dbURL).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("required")
					}
					return nil
				}),
			huh.NewSelect[string]().
				Title("JWT secret").
				Options(
					huh.NewOption("Generate random secret", "generate"),
					huh.NewOption("Enter my own", "custom"),
				).
				Value(&jwtChoice),
		),
	).Run()
	if err != nil {
		return err
	}

	if jwtChoice == "generate" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generating JWT secret: %w", err)
		}
		cfg.jwtSecret = base64.StdEncoding.EncodeToString(b)
	} else {
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("JWT_SECRET").
					Value(&cfg.jwtSecret).
					Validate(func(s string) error {
						if s == "" {
							return fmt.Errorf("required")
						}
						return nil
					}),
			),
		).Run()
		if err != nil {
			return err
		}
	}

	err = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Vault backend").
				Options(
					huh.NewOption("Local keyfile", "local"),
					huh.NewOption("GCP Secret Manager", "gcp"),
				).
				Value(&cfg.vault),
			huh.NewInput().
				Title("Server host").
				Value(&cfg.host),
			huh.NewInput().
				Title("Server port").
				Value(&cfg.port),
		),
	).Run()
	if err != nil {
		return err
	}

	if cfg.vault == "gcp" {
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("GCP Project ID").
					Value(&cfg.gcpProject).
					Validate(func(s string) error {
						if s == "" {
							return fmt.Errorf("required")
						}
						return nil
					}),
			),
		).Run()
		if err != nil {
			return err
		}
	}

	return nil
}

func stepGoogle(cfg *config) error {
	fmt.Println(section.Render("── Google Services ────────────────────────"))
	fmt.Println()

	err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Enable Gmail, Calendar, Drive, and Contacts?").
				Description("Requires a Google Cloud OAuth 2.0 client.\nSetup guide: https://github.com/clawvisor/clawvisor/blob/main/docs/GOOGLE_OAUTH_SETUP.md\nhttps://console.cloud.google.com/apis/credentials").
				Affirmative("Yes").
				Negative("No").
				Value(&cfg.googleEnabled),
		),
	).Run()
	if err != nil {
		return err
	}

	if !cfg.googleEnabled {
		return nil
	}

	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Google Client ID").
				Value(&cfg.googleID).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("required")
					}
					return nil
				}),
			huh.NewInput().
				Title("Google Client Secret").
				EchoMode(huh.EchoModePassword).
				Value(&cfg.googleSecret).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("required")
					}
					return nil
				}),
		),
	).Run()
}

func stepIMessage(cfg *config) error {
	if runtime.GOOS != "darwin" {
		cfg.imessageEnabled = false
		return nil
	}

	fmt.Println(section.Render("── iMessage ──────────────────────────────"))
	fmt.Println()

	cfg.imessageEnabled = true
	return huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Enable iMessage integration?").
				Description("Requires macOS with Messages.app configured.").
				Affirmative("Yes").
				Negative("No").
				Value(&cfg.imessageEnabled),
		),
	).Run()
}

func stepLLM(cfg *config) error {
	fmt.Println(section.Render("── LLM Configuration ──────────────────────"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("Clawvisor uses an LLM for intent verification,"))
	fmt.Println(dim.Padding(0, 2).Render("task risk assessment, and chain context tracking."))
	fmt.Println(dim.Padding(0, 2).Render("All features share the same provider. A fast"))
	fmt.Println(dim.Padding(0, 2).Render("model is recommended."))
	fmt.Println()

	var model string
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Which model?").
				Options(
					huh.NewOption("Quick start (free) — Claude Haiku, no API key needed", "quickstart"),
					huh.NewOption("Claude Haiku — claude-haiku-4-5-20251001", "haiku"),
					huh.NewOption("Gemini Flash — gemini-2.0-flash", "flash"),
					huh.NewOption("GPT-4o Mini  — gpt-4o-mini", "mini"),
					huh.NewOption("Other        — enter model ID", "other"),
				).
				Value(&model),
		),
	).Run()
	if err != nil {
		return err
	}

	switch model {
	case "quickstart":
		cfg.llmProvider = "anthropic"
		cfg.llmEndpoint = haikuproxy.BaseURL()
		cfg.llmModel = "claude-haiku-4-5-20251001"

		fmt.Println(dim.Padding(0, 2).Render("  Registering a free Haiku proxy key..."))
		reg, err := haikuproxy.Register("clawvisor-setup")
		if err != nil {
			fmt.Println(yellow.Padding(0, 2).Render(fmt.Sprintf("  Registration failed: %v", err)))
			fmt.Println(yellow.Padding(0, 2).Render("  Falling back to manual API key entry."))
			fmt.Println()
			cfg.llmEndpoint = "https://api.anthropic.com/v1"
		} else {
			cfg.llmAPIKey = reg.Key
			fmt.Println(green.Padding(0, 2).Render(fmt.Sprintf("  ✓ Registered (spend cap: $%.2f)", reg.SpendCap)))
			fmt.Println()
		}
	case "haiku":
		cfg.llmProvider = "anthropic"
		cfg.llmEndpoint = "https://api.anthropic.com/v1"
		cfg.llmModel = "claude-haiku-4-5-20251001"
	case "flash":
		cfg.llmProvider = "openai"
		cfg.llmEndpoint = "https://generativelanguage.googleapis.com/v1beta/openai"
		cfg.llmModel = "gemini-2.0-flash"
	case "mini":
		cfg.llmProvider = "openai"
		cfg.llmEndpoint = "https://api.openai.com/v1"
		cfg.llmModel = "gpt-4o-mini"
	case "other":
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Provider").
					Options(
						huh.NewOption("OpenAI-compatible", "openai"),
						huh.NewOption("Anthropic", "anthropic"),
					).
					Value(&cfg.llmProvider),
				huh.NewInput().
					Title("Endpoint URL").
					Value(&cfg.llmEndpoint).
					Validate(func(s string) error {
						if s == "" {
							return fmt.Errorf("required")
						}
						return nil
					}),
				huh.NewInput().
					Title("Model ID").
					Value(&cfg.llmModel).
					Validate(func(s string) error {
						if s == "" {
							return fmt.Errorf("required")
						}
						return nil
					}),
			),
		).Run()
		if err != nil {
			return err
		}
	}

	// Skip API key prompt if already set (e.g. from quickstart registration).
	if cfg.llmAPIKey == "" {
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("API key").
					EchoMode(huh.EchoModePassword).
					Value(&cfg.llmAPIKey).
					Validate(func(s string) error {
						if s == "" {
							return fmt.Errorf("required")
						}
						return nil
					}),
			),
		).Run()
		if err != nil {
			return err
		}
	}

	cfg.taskRiskEnabled = true
	cfg.chainContextEnabled = true
	return huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Enable task risk assessment?").
				Description("Evaluates scope and purpose coherence when tasks are created.").
				Affirmative("Yes").
				Negative("No").
				Value(&cfg.taskRiskEnabled),
			huh.NewConfirm().
				Title("Enable chain context tracking?").
				Description("Extracts entity references from results so multi-step tasks stay on-target.").
				Affirmative("Yes").
				Negative("No").
				Value(&cfg.chainContextEnabled),
		),
	).Run()
}

func stepTelemetry(cfg *config) error {
	fmt.Println(section.Render("── Telemetry ──────────────────────────────"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("Help improve Clawvisor by sharing anonymous,"))
	fmt.Println(dim.Padding(0, 2).Render("non-identifying usage data. Each report includes:"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("  • Clawvisor version (e.g. 0.5.1)"))
	fmt.Println(dim.Padding(0, 2).Render("  • OS and architecture (e.g. linux/amd64)"))
	fmt.Println(dim.Padding(0, 2).Render("  • Agent count (bucketed, e.g. \"6-20\")"))
	fmt.Println(dim.Padding(0, 2).Render("  • Requests per service (bucketed, e.g. gmail: \"101-1000\")"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("No instance ID, IP, credentials, or personal data"))
	fmt.Println(dim.Padding(0, 2).Render("is collected. Reports cannot be linked across restarts."))
	fmt.Println()

	cfg.telemetryEnabled = true
	return huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Send anonymous usage reports?").
				Affirmative("Yes").
				Negative("No").
				Value(&cfg.telemetryEnabled),
		),
	).Run()
}

func stepConfirm(cfg *config) error {
	fmt.Println(section.Render("── Summary ────────────────────────────────"))
	fmt.Println()

	var lines []string
	if cfg.envMode == "local" {
		lines = append(lines, fmt.Sprintf("  Environment    %s", bold.Render("Local")))
		lines = append(lines, fmt.Sprintf("  Database       %s", bold.Render("SQLite (./clawvisor.db)")))
	} else {
		lines = append(lines, fmt.Sprintf("  Environment    %s", bold.Render("Production")))
		lines = append(lines, fmt.Sprintf("  Database       %s", bold.Render("Postgres")))
		lines = append(lines, fmt.Sprintf("  Vault          %s", bold.Render(cfg.vault)))
	}

	if cfg.googleEnabled {
		preview := cfg.googleID
		if len(preview) > 8 {
			preview = preview[:4] + "..." + preview[len(preview)-4:]
		}
		lines = append(lines, fmt.Sprintf("  Google         %s (client ID: %s)", bold.Render("Enabled"), preview))
	} else {
		lines = append(lines, fmt.Sprintf("  Google         %s", dim.Render("Disabled")))
	}

	if cfg.imessageEnabled {
		lines = append(lines, fmt.Sprintf("  iMessage       %s", bold.Render("Enabled")))
	} else {
		lines = append(lines, fmt.Sprintf("  iMessage       %s", dim.Render("Disabled")))
	}

	lines = append(lines, fmt.Sprintf("  LLM            %s (%s)", bold.Render(cfg.llmModel), cfg.llmProvider))
	lines = append(lines, fmt.Sprintf("  Verification   %s", bold.Render("Enabled")))
	if cfg.taskRiskEnabled {
		lines = append(lines, fmt.Sprintf("  Task Risk      %s", bold.Render("Enabled")))
	} else {
		lines = append(lines, fmt.Sprintf("  Task Risk      %s", dim.Render("Disabled")))
	}
	if cfg.chainContextEnabled {
		lines = append(lines, fmt.Sprintf("  Chain Context  %s", bold.Render("Enabled")))
	} else {
		lines = append(lines, fmt.Sprintf("  Chain Context  %s", dim.Render("Disabled")))
	}

	if cfg.telemetryEnabled {
		lines = append(lines, fmt.Sprintf("  Telemetry      %s", bold.Render("Enabled")))
	} else {
		lines = append(lines, fmt.Sprintf("  Telemetry      %s", dim.Render("Disabled")))
	}

	fmt.Println(strings.Join(lines, "\n"))
	fmt.Println()

	configPath := filepath.Join(cfg.outputDir, "config.yaml")
	overwrite := false
	if _, err := os.Stat(configPath); err == nil {
		err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("config.yaml already exists. Overwrite?").
					Affirmative("Yes").
					Negative("No").
					Value(&overwrite),
			),
		).Run()
		if err != nil {
			return err
		}
		if !overwrite {
			fmt.Println("  Aborted. Existing config.yaml was not modified.")
			os.Exit(0)
		}
	} else {
		write := true
		err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Write to config.yaml?").
					Affirmative("Yes").
					Negative("No").
					Value(&write),
			),
		).Run()
		if err != nil {
			return err
		}
		if !write {
			fmt.Println("  Aborted.")
			os.Exit(0)
		}
	}

	return nil
}

func writeConfig(cfg *config, path string) error {
	var b strings.Builder

	// When writing into a daemon data dir, resolve paths relative to it.
	sqlitePath := "./clawvisor.db"
	vaultKeyPath := "./vault.key"
	frontendDir := "./web/dist"
	if cfg.outputDir != "" {
		sqlitePath = filepath.Join(cfg.outputDir, "clawvisor.db")
		vaultKeyPath = filepath.Join(cfg.outputDir, "vault.key")
		frontendDir = "" // daemon mode — no frontend served
	}

	fmt.Fprintf(&b, "server:\n")
	fmt.Fprintf(&b, "  port: %s\n", cfg.port)
	fmt.Fprintf(&b, "  host: \"%s\"\n", cfg.host)
	if frontendDir != "" {
		fmt.Fprintf(&b, "  frontend_dir: \"%s\"\n", frontendDir)
	}

	fmt.Fprintf(&b, "\ndatabase:\n")
	if cfg.envMode == "local" {
		fmt.Fprintf(&b, "  driver: \"sqlite\"\n")
		fmt.Fprintf(&b, "  sqlite_path: \"%s\"\n", sqlitePath)
	} else {
		fmt.Fprintf(&b, "  driver: \"postgres\"\n")
		fmt.Fprintf(&b, "  postgres_url: \"%s\"\n", cfg.dbURL)
	}

	fmt.Fprintf(&b, "\nvault:\n")
	fmt.Fprintf(&b, "  backend: \"%s\"\n", cfg.vault)
	if cfg.vault == "gcp" {
		fmt.Fprintf(&b, "  gcp_project: \"%s\"\n", cfg.gcpProject)
	} else {
		fmt.Fprintf(&b, "  local_key_file: \"%s\"\n", vaultKeyPath)
	}

	if cfg.envMode == "production" {
		fmt.Fprintf(&b, "\nauth:\n")
		fmt.Fprintf(&b, "  jwt_secret: \"%s\"\n", cfg.jwtSecret)
	}

	fmt.Fprintf(&b, "\nservices:\n")
	fmt.Fprintf(&b, "  google:\n")
	fmt.Fprintf(&b, "    client_id: \"%s\"\n", cfg.googleID)
	fmt.Fprintf(&b, "    client_secret: \"%s\"\n", cfg.googleSecret)
	fmt.Fprintf(&b, "  imessage:\n")
	fmt.Fprintf(&b, "    enabled: %t\n", cfg.imessageEnabled)

	fmt.Fprintf(&b, "\nllm:\n")
	fmt.Fprintf(&b, "  provider: %s\n", cfg.llmProvider)
	fmt.Fprintf(&b, "  endpoint: %s\n", cfg.llmEndpoint)
	fmt.Fprintf(&b, "  api_key: \"%s\"\n", cfg.llmAPIKey)
	fmt.Fprintf(&b, "  model: %s\n", cfg.llmModel)
	fmt.Fprintf(&b, "  verification:\n")
	fmt.Fprintf(&b, "    enabled: true\n")
	fmt.Fprintf(&b, "    timeout_seconds: 5\n")
	fmt.Fprintf(&b, "    fail_closed: true\n")
	fmt.Fprintf(&b, "    cache_ttl_seconds: 60\n")
	fmt.Fprintf(&b, "  task_risk:\n")
	fmt.Fprintf(&b, "    enabled: %t\n", cfg.taskRiskEnabled)
	fmt.Fprintf(&b, "  chain_context:\n")
	fmt.Fprintf(&b, "    enabled: %t\n", cfg.chainContextEnabled)

	fmt.Fprintf(&b, "\ntelemetry:\n")
	fmt.Fprintf(&b, "  enabled: %t\n", cfg.telemetryEnabled)

	return os.WriteFile(path, []byte(b.String()), 0644)
}

func writeTUIConfig(cfg *config) error {
	host := cfg.host
	if host == "0.0.0.0" || host == "127.0.0.1" || host == "" {
		host = "localhost"
	}
	serverURL := fmt.Sprintf("http://%s:%s", host, cfg.port)

	tuiCfg := &tuiconfig.Config{
		Profiles: map[string]tuiconfig.Profile{
			"default": {
				ServerURL: serverURL,
				Token:     "",
			},
		},
		ActiveProfile: "default",
		PollInterval:  5,
	}

	cfgPath, err := tuiconfig.DefaultPath()
	if err != nil {
		return err
	}
	return tuiCfg.Save(cfgPath)
}

// ensureVaultKey generates a vault key file if it doesn't already exist.
func ensureVaultKey(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // already exists
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generating key: %w", err)
	}
	return os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0600)
}

func printNextSteps(cfg *config) {
	fmt.Println()
	fmt.Println(green.Padding(0, 2).Render("config.yaml written"))
	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Next steps:"))
	fmt.Println(section.Render("─────────────────────────────────────────"))

	if cfg.envMode == "local" {
		fmt.Println(`  1. Start the server:
     make run

  2. The dashboard will open automatically
     in your browser.

  3. Activate services (GitHub, Slack, etc.)
     from the Services page using your API
     keys.`)
	} else {
		fmt.Println(`  1. Build the binary:
     make build

  2. Set environment variables:
     export DATABASE_URL="<your postgres url>"
     export JWT_SECRET="<your jwt secret>"
     export VAULT_KEY="$(openssl rand -base64 32)"

  3. Run the server:
     ./bin/clawvisor server

  4. For Cloud Run deployment:
     make deploy`)
	}
	fmt.Println()
}
