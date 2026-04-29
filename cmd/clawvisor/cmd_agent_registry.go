package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
	AgentToken string
	BaseURL    string
}

var agentRegisterJSON bool

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
			return err
		}

		if agentRegisterJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(entry)
		}

		fmt.Printf("Registered agent %q for future `--agent` use.\n", name)
		fmt.Printf("Stored token for %s at %s\n", cl.BaseURL(), path)
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
	agentCmd.AddCommand(agentRegisterCmd)
}
