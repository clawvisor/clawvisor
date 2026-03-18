package daemon

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
)

var (
	bold    = lipgloss.NewStyle().Bold(true)
	dim     = lipgloss.NewStyle().Faint(true)
	green   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	section = lipgloss.NewStyle().Faint(true).Padding(0, 2)
)

type daemonConfig struct {
	llmProvider string
	llmEndpoint string
	llmModel    string
	llmAPIKey   string

	taskRiskEnabled     bool
	chainContextEnabled bool

	telemetryEnabled bool
}

// runDaemonSetup runs the streamlined daemon setup wizard and writes
// config.yaml + vault.key into dataDir. Everything that can be
// auto-configured (SQLite, local vault, host/port, JWT) is hardcoded.
func runDaemonSetup(dataDir string) error {
	configPath := filepath.Join(dataDir, "config.yaml")

	printDaemonBanner()

	cfg, err := collectDaemonConfig()
	if err != nil {
		if err == huh.ErrUserAborted {
			fmt.Println("\n  Aborted. No files were written.")
			return nil
		}
		return err
	}

	// Generate JWT secret.
	jwtSecret, err := generateRandomBase64(32)
	if err != nil {
		return fmt.Errorf("generating JWT secret: %w", err)
	}

	// Write config.
	if err := writeDaemonConfig(cfg, dataDir, jwtSecret, configPath); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	// Generate vault key.
	vaultKeyPath := filepath.Join(dataDir, "vault.key")
	if err := ensureVaultKey(vaultKeyPath); err != nil {
		return fmt.Errorf("generating vault key: %w", err)
	}

	// Generate relay keys (Ed25519 for auth, X25519 for E2E).
	if err := ensureRelayKeys(dataDir); err != nil {
		return fmt.Errorf("generating relay keys: %w", err)
	}

	// Register with relay to get daemon_id.
	relayURL := os.Getenv("CLAWVISOR_RELAY_URL")
	if relayURL == "" {
		relayURL = "wss://relay.clawvisor.com"
	}
	if err := registerWithRelay(dataDir, relayURL); err != nil {
		fmt.Println(dim.Padding(0, 2).Render("  Warning: relay registration failed: " + err.Error()))
		fmt.Println(dim.Padding(0, 2).Render("  Daemon will work locally. Re-run setup to try again."))
	}

	fmt.Println()
	fmt.Println(green.Padding(0, 2).Render("✓ Daemon configured"))
	fmt.Println(dim.Padding(0, 2).Render("  " + configPath))
	fmt.Println()
	return nil
}

func printDaemonBanner() {
	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Clawvisor Daemon Setup"))
	fmt.Println(section.Render("─────────────────────────────────────────"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("The daemon runs locally on this machine."))
	fmt.Println(dim.Padding(0, 2).Render("SQLite, vault, and networking are auto-configured."))
	fmt.Println(dim.Padding(0, 2).Render("You just need to provide an LLM API key."))
	fmt.Println()
}

func collectDaemonConfig() (*daemonConfig, error) {
	cfg := &daemonConfig{}

	if err := stepDaemonLLM(cfg); err != nil {
		return nil, err
	}
	if err := stepDaemonTelemetry(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func stepDaemonLLM(cfg *daemonConfig) error {
	var model string
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Which LLM to use for verification and risk assessment?").
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

	// Build a provider-specific title and hint for the API key prompt.
	var keyTitle, keyHint string
	switch model {
	case "haiku":
		keyTitle = "Anthropic API key"
		keyHint = "Starts with sk-ant-..."
	case "flash":
		keyTitle = "Google AI API key"
		keyHint = "From Google AI Studio"
	case "mini":
		keyTitle = "OpenAI API key"
		keyHint = "Starts with sk-..."
	default:
		if cfg.llmProvider == "anthropic" {
			keyTitle = "Anthropic API key"
			keyHint = "Starts with sk-ant-..."
		} else {
			keyTitle = "API key"
			keyHint = "For " + cfg.llmEndpoint
		}
	}

	err = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title(keyTitle).
				Description(keyHint).
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

func stepDaemonTelemetry(cfg *daemonConfig) error {
	cfg.telemetryEnabled = true
	return huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Send anonymous usage reports?").
				Description("Non-identifying stats: version, OS, agent count, request counts.").
				Affirmative("Yes").
				Negative("No").
				Value(&cfg.telemetryEnabled),
		),
	).Run()
}

func writeDaemonConfig(cfg *daemonConfig, dataDir, jwtSecret, path string) error {
	var b strings.Builder

	fmt.Fprintf(&b, "# Auto-generated by clawvisor daemon setup\n")
	fmt.Fprintf(&b, "server:\n")
	fmt.Fprintf(&b, "  port: 25297\n")
	fmt.Fprintf(&b, "  host: \"127.0.0.1\"\n")

	// Resolve the frontend directory relative to the running binary.
	// This lets the daemon serve the dashboard when run from a built binary.
	if frontendDir := resolveFrontendDir(); frontendDir != "" {
		fmt.Fprintf(&b, "  frontend_dir: \"%s\"\n", frontendDir)
	}

	fmt.Fprintf(&b, "\ndatabase:\n")
	fmt.Fprintf(&b, "  driver: \"sqlite\"\n")
	fmt.Fprintf(&b, "  sqlite_path: \"%s\"\n", filepath.Join(dataDir, "clawvisor.db"))

	fmt.Fprintf(&b, "\nvault:\n")
	fmt.Fprintf(&b, "  backend: \"local\"\n")
	fmt.Fprintf(&b, "  local_key_file: \"%s\"\n", filepath.Join(dataDir, "vault.key"))

	fmt.Fprintf(&b, "\nauth:\n")
	fmt.Fprintf(&b, "  jwt_secret: \"%s\"\n", jwtSecret)

	fmt.Fprintf(&b, "\nservices:\n")
	fmt.Fprintf(&b, "  imessage:\n")
	if runtime.GOOS == "darwin" {
		fmt.Fprintf(&b, "    enabled: true\n")
	} else {
		fmt.Fprintf(&b, "    enabled: false\n")
	}

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

	relayURL := os.Getenv("CLAWVISOR_RELAY_URL")
	if relayURL == "" {
		relayURL = "wss://relay.clawvisor.com"
	}
	pushURL := os.Getenv("CLAWVISOR_PUSH_URL")
	if pushURL == "" {
		pushURL = "https://push.clawvisor.com"
	}
	fmt.Fprintf(&b, "\npush:\n")
	fmt.Fprintf(&b, "  enabled: true\n")
	fmt.Fprintf(&b, "  url: \"%s\"\n", pushURL)

	fmt.Fprintf(&b, "\nrelay:\n")
	fmt.Fprintf(&b, "  enabled: true\n")
	fmt.Fprintf(&b, "  url: \"%s\"\n", relayURL)

	return os.WriteFile(path, []byte(b.String()), 0644)
}

// resolveFrontendDir finds web/dist relative to the running binary.
// Returns "" if it can't be found (e.g. the frontend hasn't been built).
func resolveFrontendDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return ""
	}
	// The binary is typically at <repo>/clawvisor or <repo>/bin/clawvisor.
	// Walk up to find web/dist.
	dir := filepath.Dir(exe)
	for i := 0; i < 3; i++ {
		candidate := filepath.Join(dir, "web", "dist")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

func generateRandomBase64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// ensureVaultKey generates a vault key file if it doesn't already exist.
func ensureVaultKey(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	key, err := generateRandomBase64(32)
	if err != nil {
		return fmt.Errorf("generating key: %w", err)
	}
	return os.WriteFile(path, []byte(key), 0600)
}
