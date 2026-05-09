package main

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/spf13/cobra"
)

const (
	liteProxyProviderClaude = "claude"
	liteProxyProviderCodex  = "codex"
)

var liteRunProvider string

var agentLiteEnvCmd = &cobra.Command{
	Use:   "lite-env <claude|codex>",
	Short: "Print proxy-lite environment exports for an agent harness",
	Long:  "Print shell exports that route Claude Code or Codex through Clawvisor's proxy-lite LLM endpoint using an agent token.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := liteProxyOptionsFromFlags(args[0])
		if err != nil {
			return err
		}
		envPairs, err := buildLiteProxyEnv(opts.Provider, opts.BaseURL, opts.AgentToken)
		if err != nil {
			return err
		}
		for _, pair := range envPairs {
			key, value, _ := strings.Cut(pair, "=")
			fmt.Printf("export %s=%s\n", key, shellQuote(value))
		}
		return nil
	},
	SilenceUsage: true,
}

var agentLiteRunCmd = &cobra.Command{
	Use:   "lite-run -- <command> [args...]",
	Short: "Run an agent harness through proxy-lite",
	Long:  "Infer the harness from the command name, inject proxy-lite LLM endpoint environment variables, and run Claude Code or Codex. Use --provider when the command name is a wrapper script.",
	Args:  cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		provider, commandArgs, err := liteProxyRunPlan(args, liteRunProvider)
		if err != nil {
			return err
		}
		opts, err := liteProxyOptionsFromFlags(provider)
		if err != nil {
			return err
		}
		return runLiteProxyCommand(opts, commandArgs)
	},
	SilenceUsage: true,
}

func newLiteProxyHarnessCmd(provider string) *cobra.Command {
	return &cobra.Command{
		Use:   provider + " -- [args...]",
		Short: "Run " + provider + " through proxy-lite",
		Long:  "Run " + provider + " with environment variables that route its LLM calls through Clawvisor's proxy-lite endpoint.",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := liteProxyOptionsFromFlags(provider)
			if err != nil {
				return err
			}
			commandArgs := append([]string{provider}, args...)
			return runLiteProxyCommand(opts, commandArgs)
		},
		SilenceUsage: true,
	}
}

type liteProxyOptions struct {
	Provider   string
	BaseURL    string
	AgentToken string
}

func liteProxyOptionsFromFlags(provider string) (*liteProxyOptions, error) {
	normalizedProvider, err := normalizeLiteProxyProvider(provider)
	if err != nil {
		return nil, err
	}
	creds, err := resolveAgentCredentials(runtimeAgentName, runtimeAgentToken, runtimeServerURL)
	if err != nil {
		return nil, err
	}
	return &liteProxyOptions{
		Provider:   normalizedProvider,
		BaseURL:    normalizeLiteProxyServerURL(creds.BaseURL),
		AgentToken: creds.AgentToken,
	}, nil
}

func normalizeLiteProxyProvider(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case liteProxyProviderClaude:
		return liteProxyProviderClaude, nil
	case liteProxyProviderCodex:
		return liteProxyProviderCodex, nil
	default:
		return "", fmt.Errorf("unsupported proxy-lite provider %q: expected claude or codex", provider)
	}
}

func liteProxyRunPlan(args []string, explicitProvider string) (provider string, commandArgs []string, err error) {
	explicitProvider = strings.TrimSpace(explicitProvider)
	if explicitProvider != "" {
		provider, err = normalizeLiteProxyProvider(explicitProvider)
		if err != nil {
			return "", nil, err
		}
		if len(args) == 0 {
			return provider, []string{provider}, nil
		}
		return provider, args, nil
	}

	provider, ok := detectLiteProxyProviderFromCommand(args)
	if !ok {
		return "", nil, fmt.Errorf("could not infer proxy-lite provider from command %q: use claude, codex, or pass --provider", firstLiteProxyArg(args))
	}
	return provider, args, nil
}

func detectLiteProxyProviderFromCommand(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	switch normalizeLiteProxyCommandKey(args[0]) {
	case "claude", "claude-code":
		return liteProxyProviderClaude, true
	case "codex":
		return liteProxyProviderCodex, true
	default:
		return "", false
	}
}

func normalizeLiteProxyCommandKey(value string) string {
	value = strings.TrimSpace(strings.ToLower(path.Base(value)))
	return strings.TrimSuffix(value, ".exe")
}

func firstLiteProxyArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func buildLiteProxyEnv(provider, baseURL, agentToken string) ([]string, error) {
	provider, err := normalizeLiteProxyProvider(provider)
	if err != nil {
		return nil, err
	}
	baseURL = normalizeLiteProxyServerURL(baseURL)
	agentToken = strings.TrimSpace(agentToken)
	if baseURL == "" {
		return nil, fmt.Errorf("clawvisor server URL is required")
	}
	if agentToken == "" {
		return nil, fmt.Errorf("agent token is required")
	}

	env := []string{
		"CLAWVISOR_URL=" + baseURL,
		"CLAWVISOR_AGENT_TOKEN=" + agentToken,
		"CLAWVISOR_PROXY_LITE=1",
		"CLAWVISOR_PROXY_LITE_PROVIDER=" + provider,
	}
	switch provider {
	case liteProxyProviderClaude:
		env = append(env,
			"ANTHROPIC_BASE_URL="+baseURL,
			"ANTHROPIC_AUTH_TOKEN="+agentToken,
			"ANTHROPIC_API_KEY=",
		)
	case liteProxyProviderCodex:
		env = append(env,
			"OPENAI_BASE_URL="+liteProxyOpenAIBaseURL(baseURL),
			"OPENAI_API_KEY="+agentToken,
		)
	}
	return env, nil
}

func normalizeLiteProxyServerURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

func liteProxyOpenAIBaseURL(baseURL string) string {
	baseURL = normalizeLiteProxyServerURL(baseURL)
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL
	}
	return baseURL + "/v1"
}

func runLiteProxyCommand(opts *liteProxyOptions, commandArgs []string) error {
	if opts == nil {
		return fmt.Errorf("proxy-lite options are required")
	}
	if len(commandArgs) == 0 || strings.TrimSpace(commandArgs[0]) == "" {
		return fmt.Errorf("command is required")
	}
	envPairs, err := buildLiteProxyEnv(opts.Provider, opts.BaseURL, opts.AgentToken)
	if err != nil {
		return err
	}
	commandArgs = prepareLiteProxyCommandArgs(opts, commandArgs)
	child := exec.Command(commandArgs[0], commandArgs[1:]...)
	child.Env = mergeEnvironment(os.Environ(), envPairs)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	fmt.Fprintf(os.Stderr, "Routing %s through Clawvisor proxy-lite at %s\n", opts.Provider, opts.BaseURL)
	return child.Run()
}

func prepareLiteProxyCommandArgs(opts *liteProxyOptions, commandArgs []string) []string {
	if opts == nil || opts.Provider != liteProxyProviderCodex || len(commandArgs) == 0 {
		return commandArgs
	}
	if normalizeLiteProxyCommandKey(commandArgs[0]) != liteProxyProviderCodex {
		return commandArgs
	}
	injected := []string{
		commandArgs[0],
		"-c", "model_provider=clawvisor",
		"-c", `model_providers.clawvisor.name="clawvisor"`,
		"-c", fmt.Sprintf(`model_providers.clawvisor.base_url=%q`, liteProxyOpenAIBaseURL(opts.BaseURL)),
		"-c", `model_providers.clawvisor.env_key="CLAWVISOR_AGENT_TOKEN"`,
	}
	return append(injected, commandArgs[1:]...)
}

func addLiteProxyFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&runtimeAgentName, "agent", "", "Registered agent name (see clawvisor agent register)")
	cmd.Flags().StringVar(&runtimeAgentToken, "agent-token", "", "Agent bearer token (defaults to CLAWVISOR_AGENT_TOKEN)")
	cmd.Flags().StringVar(&runtimeServerURL, "url", "", "Clawvisor server URL (overrides the registered agent URL, otherwise defaults to CLAWVISOR_URL or http://127.0.0.1:25297)")
	cmd.MarkFlagsMutuallyExclusive("agent", "agent-token")
}

func init() {
	agentClaudeCmd := newLiteProxyHarnessCmd(liteProxyProviderClaude)
	agentCodexCmd := newLiteProxyHarnessCmd(liteProxyProviderCodex)
	for _, subcmd := range []*cobra.Command{agentLiteEnvCmd, agentLiteRunCmd, agentClaudeCmd, agentCodexCmd} {
		addLiteProxyFlags(subcmd)
	}
	agentLiteRunCmd.Flags().StringVar(&liteRunProvider, "provider", "", "Proxy-lite provider when it cannot be inferred from the command name (claude or codex)")
	agentCmd.AddCommand(agentLiteEnvCmd)
	agentCmd.AddCommand(agentLiteRunCmd)
	agentCmd.AddCommand(agentClaudeCmd)
	agentCmd.AddCommand(agentCodexCmd)
}
