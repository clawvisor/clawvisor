package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/clawvisor/clawvisor/internal/daemon"
	"github.com/clawvisor/clawvisor/internal/tui/client"
)

const defaultClawvisorServerURL = "http://127.0.0.1:25297"

type agentRegistry struct {
	Agents map[string]registeredAgent `json:"agents"`
}

type registeredAgent struct {
	Alias        string    `json:"alias"`
	AgentID      string    `json:"agent_id"`
	AgentName    string    `json:"agent_name"`
	ServerURL    string    `json:"server_url"`
	Token        string    `json:"token"`
	RegisteredAt time.Time `json:"registered_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type resolvedAgentCredentials struct {
	Alias      string
	AgentID    string
	AgentName  string
	AgentToken string
	BaseURL    string
}

var agentRegisterJSON bool
var agentRegisterProvider string
var agentRegisterAPIKey string
var agentRegisterSkipLLMKey bool

var agentRegisterCmd = &cobra.Command{
	Use:   "register <name>",
	Short: "Create or rotate a named agent, then store its token locally for --agent use",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(args[0])
		if name == "" {
			return fmt.Errorf("agent name is required")
		}

		cl, err := daemon.NewAPIClient()
		if err != nil {
			return err
		}
		agent, err := createOrRotateAgentByName(cl, name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(agent.Token) == "" {
			return fmt.Errorf("agent %q did not return a token", name)
		}

		now := time.Now().UTC()
		path, err := agentRegistryPath()
		if err != nil {
			return err
		}
		registry, err := loadAgentRegistry(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if registry == nil {
			registry = &agentRegistry{Agents: map[string]registeredAgent{}}
		}

		registeredAt := now
		if existing, ok := registry.Agents[name]; ok && !existing.RegisteredAt.IsZero() {
			registeredAt = existing.RegisteredAt
		}
		entry := registeredAgent{
			Alias:        name,
			AgentID:      agent.ID,
			AgentName:    agent.Name,
			ServerURL:    cl.BaseURL(),
			Token:        agent.Token,
			RegisteredAt: registeredAt,
			UpdatedAt:    now,
		}
		registry.Agents[name] = entry
		if err := saveAgentRegistry(path, registry); err != nil {
			return fmt.Errorf("rotated token for agent %q but could not save local registry: %w\nrecovery:\n  server_url=%s\n  agent_token=%s", name, err, cl.BaseURL(), agent.Token)
		}

		llmSetup, err := maybeConfigureRegisteredAgentLLM(cmd, cl, entry)
		if err != nil {
			return err
		}

		if agentRegisterJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(entry)
		}

		fmt.Printf("Registered agent %q for future `--agent` use.\n", name)
		fmt.Printf("Stored token for %s at %s\n", cl.BaseURL(), path)
		printAgentRegisterNextSteps(os.Stdout, entry, llmSetup)
		return nil
	},
	SilenceUsage: true,
}

func createOrRotateAgentByName(cl *client.Client, name string) (*client.Agent, error) {
	agents, err := cl.GetAgents()
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}
	for _, a := range agents {
		if a.Name == name {
			rotated, err := cl.RotateAgentToken(a.ID)
			if err != nil {
				return nil, fmt.Errorf("rotating agent token: %w", err)
			}
			return rotated, nil
		}
	}
	created, err := cl.CreateAgentWithOpts(name, false)
	if err != nil {
		return nil, fmt.Errorf("creating agent: %w", err)
	}
	return created, nil
}

func agentRegistryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".clawvisor", "agents.json"), nil
}

func loadAgentRegistry(path string) (*agentRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var registry agentRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("parsing agent registry: %w", err)
	}
	if registry.Agents == nil {
		registry.Agents = map[string]registeredAgent{}
	}
	return &registry, nil
}

func saveAgentRegistry(path string, registry *agentRegistry) error {
	if registry == nil {
		return fmt.Errorf("agent registry is required")
	}
	if registry.Agents == nil {
		registry.Agents = map[string]registeredAgent{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating agent registry directory: %w", err)
	}
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding agent registry: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing agent registry: %w", err)
	}
	return nil
}

type agentRegisterLLMSetup struct {
	Provider string
	Stored   bool
	Skipped  bool
}

func maybeConfigureRegisteredAgentLLM(cmd *cobra.Command, cl *client.Client, entry registeredAgent) (*agentRegisterLLMSetup, error) {
	if agentRegisterSkipLLMKey {
		return &agentRegisterLLMSetup{Skipped: true}, nil
	}

	provider, err := normalizeAgentRegisterLLMProvider(agentRegisterProvider)
	if err != nil {
		return nil, err
	}
	apiKey := strings.TrimSpace(agentRegisterAPIKey)
	interactive := !agentRegisterJSON && cmd != nil && cmd.InOrStdin() != nil && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))

	if provider == "" && apiKey != "" {
		return nil, fmt.Errorf("--api-key requires --provider")
	}

	if provider == "" {
		if !interactive {
			return &agentRegisterLLMSetup{Skipped: true}, nil
		}
		provider = "anthropic"
		if err := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Which upstream LLM provider should proxy-lite use for this agent?").
					Options(
						huh.NewOption("Anthropic (Claude Code)", "anthropic"),
						huh.NewOption("OpenAI (Codex)", "openai"),
						huh.NewOption("Skip for now", "skip"),
					).
					Value(&provider),
			),
		).Run(); err != nil {
			return nil, err
		}
		if provider == "skip" {
			return &agentRegisterLLMSetup{Skipped: true}, nil
		}
	}

	if apiKey == "" {
		if !interactive {
			return nil, fmt.Errorf("--provider requires --api-key when stdin/stdout are not interactive")
		}
		if err := huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title(agentRegisterAPIKeyPromptTitle(provider)).
					EchoMode(huh.EchoModePassword).
					Value(&apiKey),
			),
		).Run(); err != nil {
			return nil, err
		}
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			return nil, fmt.Errorf("API key is required")
		}
	}

	if _, err := cl.SetLLMCredential(provider, apiKey, entry.AgentID); err != nil {
		return nil, fmt.Errorf("storing %s upstream API key for agent %q: %w", provider, entry.Alias, err)
	}
	return &agentRegisterLLMSetup{Provider: provider, Stored: true}, nil
}

func normalizeAgentRegisterLLMProvider(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "":
		return "", nil
	case "anthropic", "claude", "claude-code":
		return "anthropic", nil
	case "openai", "codex":
		return "openai", nil
	default:
		return "", fmt.Errorf("unsupported provider %q: expected anthropic or openai", provider)
	}
}

func agentRegisterAPIKeyPromptTitle(provider string) string {
	switch provider {
	case "anthropic":
		return "Anthropic API key"
	case "openai":
		return "OpenAI API key"
	default:
		return "Upstream API key"
	}
}

func agentRegisterHarnessForProvider(provider string) string {
	switch provider {
	case "anthropic":
		return liteProxyProviderClaude
	case "openai":
		return liteProxyProviderCodex
	default:
		return ""
	}
}

func printAgentRegisterNextSteps(out io.Writer, entry registeredAgent, setup *agentRegisterLLMSetup) {
	if out == nil {
		return
	}
	alias := entry.Alias
	if alias == "" {
		alias = entry.AgentName
	}
	if alias == "" {
		return
	}
	fmt.Fprintln(out)
	if setup != nil && setup.Stored {
		fmt.Fprintf(out, "Stored %s upstream API key for this agent.\n", setup.Provider)
	}
	aliasArg := shellQuoteIfNeeded(alias)
	if harness := ""; setup != nil {
		harness = agentRegisterHarnessForProvider(setup.Provider)
		if harness != "" {
			fmt.Fprintln(out, "Connect through proxy-lite:")
			fmt.Fprintf(out, "  clawvisor agent %s --agent %s -- %s\n", harness, aliasArg, sampleLiteProxyPrompt(harness))
			fmt.Fprintf(out, "  clawvisor agent lite-env %s --agent %s\n", harness, aliasArg)
			return
		}
	}
	fmt.Fprintln(out, "Connect through proxy-lite after storing an upstream key:")
	fmt.Fprintf(out, "  clawvisor agent register %s --provider anthropic --api-key \"$ANTHROPIC_API_KEY\"\n", aliasArg)
	fmt.Fprintf(out, "  clawvisor agent register %s --provider openai --api-key \"$OPENAI_API_KEY\"\n", aliasArg)
	fmt.Fprintf(out, "  clawvisor agent claude --agent %s -- --print \"what is 2+2\"\n", aliasArg)
	fmt.Fprintf(out, "  clawvisor agent codex --agent %s -- exec \"say hi\"\n", aliasArg)
	fmt.Fprintf(out, "  clawvisor agent lite-env claude --agent %s\n", aliasArg)
}

func shellQuoteIfNeeded(value string) string {
	if value == "" {
		return shellQuote(value)
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '/', r == ':':
		default:
			return shellQuote(value)
		}
	}
	return value
}

func sampleLiteProxyPrompt(harness string) string {
	switch harness {
	case liteProxyProviderClaude:
		return "--print \"what is 2+2\""
	case liteProxyProviderCodex:
		return "exec \"say hi\""
	default:
		return "\"say hi\""
	}
}

func resolveAgentCredentials(agentName, agentToken, baseURL string) (*resolvedAgentCredentials, error) {
	name := strings.TrimSpace(agentName)
	token := strings.TrimSpace(agentToken)
	resolvedURL := strings.TrimSpace(baseURL)

	if name != "" && token != "" {
		return nil, fmt.Errorf("--agent and --agent-token are mutually exclusive")
	}

	if name != "" {
		path, err := agentRegistryPath()
		if err != nil {
			return nil, err
		}
		registry, err := loadAgentRegistry(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("registered agent %q not found: run `clawvisor agent register %s`", name, name)
			}
			return nil, err
		}
		entry, ok := registry.Agents[name]
		if !ok || strings.TrimSpace(entry.Token) == "" {
			return nil, fmt.Errorf("registered agent %q not found: run `clawvisor agent register %s`", name, name)
		}
		if resolvedURL == "" {
			resolvedURL = strings.TrimSpace(entry.ServerURL)
		}
		if resolvedURL == "" {
			resolvedURL = strings.TrimSpace(os.Getenv("CLAWVISOR_URL"))
		}
		if resolvedURL == "" {
			resolvedURL = defaultClawvisorServerURL
		}
		return &resolvedAgentCredentials{
			Alias:      name,
			AgentID:    entry.AgentID,
			AgentName:  entry.AgentName,
			AgentToken: entry.Token,
			BaseURL:    resolvedURL,
		}, nil
	}

	if token == "" {
		token = strings.TrimSpace(os.Getenv("CLAWVISOR_AGENT_TOKEN"))
	}
	if token == "" {
		return nil, fmt.Errorf("agent credentials are required: pass --agent, pass --agent-token, or set CLAWVISOR_AGENT_TOKEN")
	}
	if resolvedURL == "" {
		resolvedURL = strings.TrimSpace(os.Getenv("CLAWVISOR_URL"))
	}
	if resolvedURL == "" {
		resolvedURL = defaultClawvisorServerURL
	}
	return &resolvedAgentCredentials{
		AgentToken: token,
		BaseURL:    resolvedURL,
	}, nil
}

func init() {
	agentRegisterCmd.Flags().BoolVar(&agentRegisterJSON, "json", false, "Output the registered agent record in JSON format")
	agentRegisterCmd.Flags().StringVar(&agentRegisterProvider, "provider", "", "Upstream LLM provider to store for proxy-lite (anthropic or openai)")
	agentRegisterCmd.Flags().StringVar(&agentRegisterAPIKey, "api-key", "", "Upstream provider API key to store for this agent (prefer the interactive prompt when possible)")
	agentRegisterCmd.Flags().BoolVar(&agentRegisterSkipLLMKey, "skip-llm-key", false, "Do not prompt for or store an upstream provider API key")
	agentCmd.AddCommand(agentRegisterCmd)
}
