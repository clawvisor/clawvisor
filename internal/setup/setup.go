package setup

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	tuiconfig "github.com/clawvisor/clawvisor/internal/tui/config"
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

	verifyProvider string
	verifyEndpoint string
	verifyModel    string
	verifyAPIKey   string
}

// Run executes the setup wizard.
func Run() error {
	printBanner()

	cfg, err := runSetup()
	if err != nil {
		if err == huh.ErrUserAborted {
			fmt.Println("\n  Aborted. No files were written.")
			return nil
		}
		return err
	}

	if err := writeConfig(cfg); err != nil {
		return fmt.Errorf("writing config: %w", err)
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
		port:     "8080",
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
	if err := stepVerification(cfg); err != nil {
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

func stepVerification(cfg *config) error {
	fmt.Println(section.Render("── Intent Verification ────────────────────"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("Clawvisor uses an LLM to verify that agent"))
	fmt.Println(dim.Padding(0, 2).Render("requests match their approved task scope."))
	fmt.Println(dim.Padding(0, 2).Render("A fast, lightweight model is recommended."))
	fmt.Println()

	var model string
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Which model?").
				Options(
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
	case "haiku":
		cfg.verifyProvider = "anthropic"
		cfg.verifyEndpoint = "https://api.anthropic.com/v1"
		cfg.verifyModel = "claude-haiku-4-5-20251001"
	case "flash":
		cfg.verifyProvider = "openai"
		cfg.verifyEndpoint = "https://generativelanguage.googleapis.com/v1beta/openai"
		cfg.verifyModel = "gemini-2.0-flash"
	case "mini":
		cfg.verifyProvider = "openai"
		cfg.verifyEndpoint = "https://api.openai.com/v1"
		cfg.verifyModel = "gpt-4o-mini"
	case "other":
		err = huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Provider").
					Options(
						huh.NewOption("OpenAI-compatible", "openai"),
						huh.NewOption("Anthropic", "anthropic"),
					).
					Value(&cfg.verifyProvider),
				huh.NewInput().
					Title("Endpoint URL").
					Value(&cfg.verifyEndpoint).
					Validate(func(s string) error {
						if s == "" {
							return fmt.Errorf("required")
						}
						return nil
					}),
				huh.NewInput().
					Title("Model ID").
					Value(&cfg.verifyModel).
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

	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("API key").
				EchoMode(huh.EchoModePassword).
				Value(&cfg.verifyAPIKey).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("required")
					}
					return nil
				}),
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

	lines = append(lines, fmt.Sprintf("  Verification   %s (%s)", bold.Render(cfg.verifyModel), cfg.verifyProvider))

	fmt.Println(strings.Join(lines, "\n"))
	fmt.Println()

	overwrite := false
	if _, err := os.Stat("config.yaml"); err == nil {
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

func writeConfig(cfg *config) error {
	var b strings.Builder

	fmt.Fprintf(&b, "server:\n")
	fmt.Fprintf(&b, "  port: %s\n", cfg.port)
	fmt.Fprintf(&b, "  host: \"%s\"\n", cfg.host)
	fmt.Fprintf(&b, "  frontend_dir: \"./web/dist\"\n")

	fmt.Fprintf(&b, "\ndatabase:\n")
	if cfg.envMode == "local" {
		fmt.Fprintf(&b, "  driver: \"sqlite\"\n")
		fmt.Fprintf(&b, "  sqlite_path: \"./clawvisor.db\"\n")
	} else {
		fmt.Fprintf(&b, "  driver: \"postgres\"\n")
		fmt.Fprintf(&b, "  postgres_url: \"%s\"\n", cfg.dbURL)
	}

	fmt.Fprintf(&b, "\nvault:\n")
	fmt.Fprintf(&b, "  backend: \"%s\"\n", cfg.vault)
	if cfg.vault == "gcp" {
		fmt.Fprintf(&b, "  gcp_project: \"%s\"\n", cfg.gcpProject)
	} else {
		fmt.Fprintf(&b, "  local_key_file: \"./vault.key\"\n")
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
	fmt.Fprintf(&b, "  verification:\n")
	fmt.Fprintf(&b, "    enabled: true\n")
	fmt.Fprintf(&b, "    provider: %s\n", cfg.verifyProvider)
	fmt.Fprintf(&b, "    endpoint: %s\n", cfg.verifyEndpoint)
	fmt.Fprintf(&b, "    api_key: \"%s\"\n", cfg.verifyAPIKey)
	fmt.Fprintf(&b, "    model: %s\n", cfg.verifyModel)
	fmt.Fprintf(&b, "    timeout_seconds: 5\n")
	fmt.Fprintf(&b, "    fail_closed: true\n")
	fmt.Fprintf(&b, "    cache_ttl_seconds: 60\n")

	return os.WriteFile("config.yaml", []byte(b.String()), 0644)
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

  3. Run the server:
     ./bin/clawvisor

  4. For Cloud Run deployment:
     make deploy`)
	}
	fmt.Println()
}
