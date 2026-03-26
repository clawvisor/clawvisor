package daemon

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"

	"github.com/clawvisor/clawvisor/pkg/version"
)

var (
	bold    = lipgloss.NewStyle().Bold(true)
	dim     = lipgloss.NewStyle().Faint(true)
	green   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellow  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	section = lipgloss.NewStyle().Faint(true).Padding(0, 2)
	warnBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("3")).
		Padding(1, 2).
		Margin(0, 2)
)

type daemonConfig struct {
	llmProvider string
	llmEndpoint string
	llmModel    string
	llmAPIKey   string

	taskRiskEnabled     bool
	chainContextEnabled bool

	telemetryEnabled bool
	relayEnabled     bool
}

// knownAgent describes a coding agent that Clawvisor can work with.
type knownAgent struct {
	Binary      string // executable name to look up via PATH
	DisplayName string // human-readable name shown in the installer
}

// knownAgents is the list of coding agents we try to detect on the user's machine.
var knownAgents = []knownAgent{
	{"claude", "Claude Code"},
	{"cursor", "Cursor"},
	{"windsurf", "Windsurf"},
	{"code", "VS Code (Copilot / Cline / etc.)"},
	{"aider", "Aider"},
}

// appBundleAgents are agents detected by checking for an installed application
// rather than a binary on PATH.
var appBundleAgents = []struct {
	knownAgent
	Paths []string // paths to check (macOS .app bundles, Linux .desktop files, etc.)
}{
	{
		knownAgent{Binary: "claude-desktop", DisplayName: "Claude Desktop"},
		[]string{
			"/Applications/Claude.app",
			"/usr/share/applications/claude.desktop", // Snap / .deb on Linux
		},
	},
}

// detectAgents returns the known agents found on $PATH or installed as app bundles.
func detectAgents() []knownAgent {
	var found []knownAgent
	for _, a := range knownAgents {
		if _, err := exec.LookPath(a.Binary); err == nil {
			found = append(found, a)
		}
	}
	for _, a := range appBundleAgents {
		for _, p := range a.Paths {
			if _, err := os.Stat(p); err == nil {
				found = append(found, a.knownAgent)
				break
			}
		}
	}
	return found
}

const agentContinueOption = "__agent_continue__"
const agentAddOption = "__agent_add__"

// stepAgents auto-detects agents and lets the user add more via an interactive
// menu (similar to the service connector). Returns the final list of agents.
func stepAgents() ([]knownAgent, error) {
	agents := detectAgents()

	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Agents"))
	fmt.Println(dim.Padding(0, 2).Render("Clawvisor detected the following coding agents on this machine."))
	fmt.Println(dim.Padding(0, 2).Render("You can add others that weren't auto-detected."))
	fmt.Println()

	if len(agents) == 0 {
		fmt.Println(dim.Padding(0, 2).Render("  No known agents detected."))
		fmt.Println()
	} else {
		for _, a := range agents {
			fmt.Println(green.Padding(0, 2).Render("  ✓ " + a.DisplayName))
		}
		fmt.Println()
	}

	for {
		selected, err := presentAgentMenu(agents)
		if err != nil {
			return agents, err
		}
		if selected == agentContinueOption {
			return agents, nil
		}
		if selected == agentAddOption {
			added, err := promptAddAgent(agents)
			if err != nil {
				if err == huh.ErrUserAborted {
					continue
				}
				return agents, err
			}
			if added != nil {
				agents = append(agents, *added)
				fmt.Println(green.Padding(0, 2).Render("  ✓ Added " + added.DisplayName))
				fmt.Println()
			}
			continue
		}
	}
}

// presentAgentMenu shows the agent list with a prominent continue option
// and an option to add more.
func presentAgentMenu(agents []knownAgent) (string, error) {
	var opts []huh.Option[string]

	// Continue is prominent at the top.
	continueLabel := bold.Render("Continue →")
	if len(agents) > 0 {
		continueLabel = bold.Render(fmt.Sprintf("Continue with %d agent(s) →", len(agents)))
	}
	opts = append(opts, huh.NewOption(continueLabel, agentContinueOption))

	// Show detected agents (informational, not selectable for action).
	for _, a := range agents {
		label := fmt.Sprintf("%s  %s", green.Render("✓"), a.DisplayName)
		opts = append(opts, huh.NewOption(label, "agent:"+a.Binary))
	}

	// Add more option.
	opts = append(opts, huh.NewOption(dim.Render("+ Add an agent"), agentAddOption))

	var choice string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Agents to configure").
				Options(opts...).
				Value(&choice),
		),
	).Run(); err != nil {
		return "", err
	}

	// If they selected an existing agent, just re-show the menu.
	if strings.HasPrefix(choice, "agent:") {
		return choice, nil
	}
	return choice, nil
}

// promptAddAgent asks the user to pick from known agents not yet in the list,
// or enter a custom agent name.
func promptAddAgent(existing []knownAgent) (*knownAgent, error) {
	have := make(map[string]bool, len(existing))
	for _, a := range existing {
		have[a.Binary] = true
	}

	var opts []huh.Option[string]

	// Show known agents that weren't detected.
	for _, a := range knownAgents {
		if !have[a.Binary] {
			opts = append(opts, huh.NewOption(a.DisplayName, a.Binary))
		}
	}
	opts = append(opts, huh.NewOption("Other (enter name)", "__custom__"))
	opts = append(opts, huh.NewOption("← Cancel", "__cancel__"))

	var choice string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Add an agent").
				Options(opts...).
				Value(&choice),
		),
	).Run(); err != nil {
		return nil, err
	}

	if choice == "__cancel__" {
		return nil, nil
	}

	if choice == "__custom__" {
		var name string
		if err := huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("Agent name").
					Description("A display name for this agent (e.g. \"Devin\")").
					Value(&name),
			),
		).Run(); err != nil {
			return nil, err
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, nil
		}
		return &knownAgent{Binary: "", DisplayName: name}, nil
	}

	// Known agent picked from the list.
	for _, a := range knownAgents {
		if a.Binary == choice {
			return &a, nil
		}
	}
	return nil, nil
}

// nonInteractive reports whether the setup wizard should run without
// interactive prompts, reading configuration from environment variables
// instead. Set CLAWVISOR_NON_INTERACTIVE=1 to enable.
//
// Required env vars in non-interactive mode:
//
//	CLAWVISOR_LLM_PROVIDER   — "anthropic" or "openai"
//	CLAWVISOR_LLM_ENDPOINT   — e.g. "https://api.anthropic.com/v1"
//	CLAWVISOR_LLM_MODEL      — e.g. "claude-haiku-4-5-20251001"
//	CLAWVISOR_LLM_API_KEY    — the API key
//
// Optional:
//
//	CLAWVISOR_TELEMETRY      — "true" or "false" (default: true)
//	CLAWVISOR_CHAIN_CONTEXT  — "true" or "false" (default: true)
func nonInteractive() bool {
	return os.Getenv("CLAWVISOR_NON_INTERACTIVE") == "1"
}

// runDaemonSetup runs the streamlined daemon setup wizard and writes
// config.yaml + vault.key into dataDir. Everything that can be
// auto-configured (SQLite, local vault, host/port, JWT) is hardcoded.
func runDaemonSetup(dataDir string) error {
	configPath := filepath.Join(dataDir, "config.yaml")

	var agents []knownAgent
	relayEnabled := true

	if nonInteractive() {
		fmt.Println(dim.Padding(0, 2).Render("Non-interactive mode: skipping interactive steps."))
		relayEnabled = os.Getenv("CLAWVISOR_RELAY") != "false"
	} else {
		// Step 0: welcome / explainer.
		if err := stepWelcome(); err != nil {
			if err == huh.ErrUserAborted {
				fmt.Println("\n  Aborted. No files were written.")
			}
			return err
		}

		// Step 1: detect and manage agents.
		var err error
		agents, err = stepAgents()
		if err != nil {
			if err == huh.ErrUserAborted {
				fmt.Println("\n  Aborted. No files were written.")
			}
			return err
		}

		// Step 2: relay opt-in.
		relayEnabled, err = stepRelayOptIn()
		if err != nil {
			if err == huh.ErrUserAborted {
				fmt.Println("\n  Aborted. No files were written.")
				return huh.ErrUserAborted
			}
			return err
		}
	}

	cfg, err := collectDaemonConfig()
	if err != nil {
		if err == huh.ErrUserAborted {
			fmt.Println("\n  Aborted. No files were written.")
			return huh.ErrUserAborted
		}
		return err
	}
	cfg.relayEnabled = relayEnabled

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

	if cfg.relayEnabled {
		// Generate relay keys (Ed25519 for auth, X25519 for E2E).
		if err := ensureRelayKeys(dataDir); err != nil {
			return fmt.Errorf("generating relay keys: %w", err)
		}

		// Register with relay to get daemon_id.
		relayURL := os.Getenv("CLAWVISOR_RELAY_URL")
		if relayURL == "" {
			relayURL = version.RelayURL()
		}
		if err := registerWithRelay(dataDir, relayURL); err != nil {
			fmt.Println(dim.Padding(0, 2).Render("  Warning: relay registration failed: " + err.Error()))
			fmt.Println(dim.Padding(0, 2).Render("  Daemon will work locally. Re-run setup to try again."))
		}
	}

	// Offer to install the Claude Code slash command if detected.
	if hasClaudeCode(agents) {
		if err := offerClaudeCodeSetup(dataDir); err != nil && err != huh.ErrUserAborted {
			fmt.Println(dim.Padding(0, 2).Render("  Warning: could not install Claude Code command: " + err.Error()))
		}
	}

	fmt.Println()
	fmt.Println(green.Padding(0, 2).Render("✓ Daemon configured"))
	fmt.Println(dim.Padding(0, 2).Render("  " + configPath))
	fmt.Println()
	return nil
}

// stepRelayOptIn asks the user whether to connect to the public relay. If they
// decline, a warning is shown. If they accept, TOS/PP acceptance is required.
func stepRelayOptIn() (enabled bool, err error) {
	useRelay := true
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Connect to the Clawvisor relay?").
				Description("The relay lets you access Clawvisor remotely and receive\nmobile push notifications. Traffic is end-to-end encrypted\nwhen your client supports it.").
				Affirmative("Yes").
				Negative("No").
				Value(&useRelay),
		),
	).Run(); err != nil {
		return false, err
	}

	if !useRelay {
		fmt.Println()
		fmt.Println(warnBox.Render(
			yellow.Bold(true).Render("⚠  Local-only mode") + "\n\n" +
				"Without the relay, agents must connect to Clawvisor\n" +
				"over your local network. You will need to:\n\n" +
				"  • Ensure agents can reach this machine's IP/port\n" +
				"  • Handle firewalls and NAT traversal yourself\n" +
				"  • Forego mobile push notifications\n\n" +
				"You can enable the relay later by re-running setup."))
		fmt.Println()

		confirmSkip := false
		if err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Continue without the relay?").
					Affirmative("Yes, local-only").
					Negative("Go back").
					Value(&confirmSkip),
			),
		).Run(); err != nil {
			return false, err
		}
		if !confirmSkip {
			// Let them reconsider — recurse back to the relay question.
			return stepRelayOptIn()
		}
		return false, nil
	}

	// Relay requires TOS/PP acceptance.
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("The relay is operated by Clawvisor. By connecting, you agree"))
	fmt.Println(dim.Padding(0, 2).Render("to the Terms of Service and Privacy Policy:"))
	fmt.Println(dim.Padding(0, 2).Render("  https://clawvisor.com/terms"))
	fmt.Println(dim.Padding(0, 2).Render("  https://clawvisor.com/privacy"))
	fmt.Println()

	accepted := false
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Do you agree to the Terms of Service and Privacy Policy?").
				Affirmative("I agree").
				Negative("Cancel").
				Value(&accepted),
		),
	).Run(); err != nil {
		return false, err
	}
	if !accepted {
		// Declining TOS brings you back to the relay question.
		return stepRelayOptIn()
	}

	return true, nil
}

func stepWelcome() error {
	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Clawvisor Setup"))
	fmt.Println(section.Render("─────────────────────────────────────────"))
	fmt.Println()

	fmt.Println(bold.Padding(0, 2).Render("How Clawvisor Works"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("Clawvisor is a credential-vaulting gateway that runs locally on"))
	fmt.Println(dim.Padding(0, 2).Render("your machine. AI coding agents don't get raw API keys — instead,"))
	fmt.Println(dim.Padding(0, 2).Render("they make requests through Clawvisor, which:"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("  • Stores credentials in an encrypted local vault"))
	fmt.Println(dim.Padding(0, 2).Render("  • Proxies agent requests to external APIs (Gmail, GitHub, Slack, …)"))
	fmt.Println(dim.Padding(0, 2).Render("  • Enforces authorization policies and requires human approval"))
	fmt.Println(dim.Padding(0, 2).Render("  • Logs an audit trail of every action an agent takes"))
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("This wizard will help you detect your agents, configure the daemon,"))
	fmt.Println(dim.Padding(0, 2).Render("and connect services."))
	fmt.Println()

	cont := true
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Ready to get started?").
				Affirmative("Continue").
				Negative("Cancel").
				Value(&cont),
		),
	).Run(); err != nil {
		return err
	}
	if !cont {
		return huh.ErrUserAborted
	}
	return nil
}

func collectDaemonConfig() (*daemonConfig, error) {
	if nonInteractive() {
		return collectDaemonConfigFromEnv()
	}

	cfg := &daemonConfig{}

	if err := stepDaemonLLM(cfg); err != nil {
		return nil, err
	}
	if err := stepDaemonTelemetry(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// collectDaemonConfigFromEnv builds a daemonConfig from environment
// variables, used in non-interactive mode.
func collectDaemonConfigFromEnv() (*daemonConfig, error) {
	cfg := &daemonConfig{
		llmProvider: os.Getenv("CLAWVISOR_LLM_PROVIDER"),
		llmEndpoint: os.Getenv("CLAWVISOR_LLM_ENDPOINT"),
		llmModel:    os.Getenv("CLAWVISOR_LLM_MODEL"),
		llmAPIKey:   os.Getenv("CLAWVISOR_LLM_API_KEY"),
	}

	if cfg.llmProvider == "" || cfg.llmEndpoint == "" || cfg.llmModel == "" || cfg.llmAPIKey == "" {
		return nil, fmt.Errorf("non-interactive mode requires CLAWVISOR_LLM_PROVIDER, CLAWVISOR_LLM_ENDPOINT, CLAWVISOR_LLM_MODEL, and CLAWVISOR_LLM_API_KEY")
	}

	cfg.taskRiskEnabled = true
	cfg.chainContextEnabled = os.Getenv("CLAWVISOR_CHAIN_CONTEXT") != "false"
	cfg.telemetryEnabled = os.Getenv("CLAWVISOR_TELEMETRY") != "false"

	fmt.Println(dim.Padding(0, 2).Render(fmt.Sprintf("  LLM: %s (%s)", cfg.llmModel, cfg.llmProvider)))
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

	// Build a provider-specific title, hint, and setup URL for the API key prompt.
	var keyTitle, keyHint, keyURL string
	switch model {
	case "haiku":
		keyTitle = "Anthropic API key"
		keyHint = "Starts with sk-ant-..."
		keyURL = "https://console.anthropic.com/settings/keys"
	case "flash":
		keyTitle = "Google AI API key"
		keyHint = "From Google AI Studio"
		keyURL = "https://aistudio.google.com/apikey"
	case "mini":
		keyTitle = "OpenAI API key"
		keyHint = "Starts with sk-..."
		keyURL = "https://platform.openai.com/api-keys"
	default:
		if cfg.llmProvider == "anthropic" {
			keyTitle = "Anthropic API key"
			keyHint = "Starts with sk-ant-..."
			keyURL = "https://console.anthropic.com/settings/keys"
		} else {
			keyTitle = "API key"
			keyHint = "For " + cfg.llmEndpoint
		}
	}

	if keyURL != "" {
		fmt.Println(dim.Padding(0, 2).Render(fmt.Sprintf("  Get your API key at: %s", keyURL)))
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
				Title("Enable chain context tracking?").
				Description("Sends API call output to your LLM provider to extract entity references\n(IDs, emails, etc.) for multi-step task validation. Disable in config.yaml later.").
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

// yamlQuote safely quotes a string for use as a YAML scalar value,
// handling special characters that would otherwise corrupt the config.
func yamlQuote(s string) string {
	out, _ := yaml.Marshal(s)
	return strings.TrimSpace(string(out))
}

func writeDaemonConfig(cfg *daemonConfig, dataDir, jwtSecret, path string) error {
	var b strings.Builder

	fmt.Fprintf(&b, "# Auto-generated by clawvisor setup\n")
	fmt.Fprintf(&b, "server:\n")
	fmt.Fprintf(&b, "  port: 25297\n")
	fmt.Fprintf(&b, "  host: \"127.0.0.1\"\n")

	// No frontend_dir — the embedded frontend FS is used automatically.

	fmt.Fprintf(&b, "\ndatabase:\n")
	fmt.Fprintf(&b, "  driver: \"sqlite\"\n")
	fmt.Fprintf(&b, "  sqlite_path: %s\n", yamlQuote(filepath.Join(dataDir, "clawvisor.db")))

	fmt.Fprintf(&b, "\nvault:\n")
	fmt.Fprintf(&b, "  backend: \"local\"\n")
	fmt.Fprintf(&b, "  local_key_file: %s\n", yamlQuote(filepath.Join(dataDir, "vault.key")))

	fmt.Fprintf(&b, "\nauth:\n")
	fmt.Fprintf(&b, "  jwt_secret: %s\n", yamlQuote(jwtSecret))

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
	fmt.Fprintf(&b, "  api_key: %s\n", yamlQuote(cfg.llmAPIKey))
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

	if cfg.relayEnabled {
		fmt.Fprintf(&b, "\npush:\n")
		fmt.Fprintf(&b, "  enabled: true\n")

		fmt.Fprintf(&b, "\nrelay:\n")
		fmt.Fprintf(&b, "  enabled: true\n")
		fmt.Fprintf(&b, "  key_file: %s\n", yamlQuote(filepath.Join(dataDir, "daemon-ed25519.key")))
		fmt.Fprintf(&b, "  e2e_key_file: %s\n", yamlQuote(filepath.Join(dataDir, "daemon-x25519.key")))
	} else {
		fmt.Fprintf(&b, "\npush:\n")
		fmt.Fprintf(&b, "  enabled: false\n")

		fmt.Fprintf(&b, "\nrelay:\n")
		fmt.Fprintf(&b, "  enabled: false\n")
	}

	return os.WriteFile(path, []byte(b.String()), 0600)
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
