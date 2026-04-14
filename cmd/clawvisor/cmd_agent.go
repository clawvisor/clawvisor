package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/internal/daemon"
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage agents (create, list, delete)",
}

// ── agent create ────────────────────────────────────────────────────────────

var agentCreateJSON bool
var agentCreateWithCallback bool
var agentCreateReplace bool

var agentCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new agent and print its token",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		cl, err := daemon.NewAPIClient()
		if err != nil {
			return err
		}

		// If --replace and an agent with the same name exists, rotate its
		// token instead of deleting and recreating. This preserves the
		// agent ID, active tasks, and group pairings.
		if agentCreateReplace {
			existing, _ := cl.GetAgents()
			for _, a := range existing {
				if a.Name == name {
					rotated, err := cl.RotateAgentToken(a.ID)
					if err != nil {
						return fmt.Errorf("rotating token: %w", err)
					}
					if agentCreateJSON {
						out := map[string]string{
							"id":    rotated.ID,
							"name":  name,
							"token": rotated.Token,
						}
						enc := json.NewEncoder(os.Stdout)
						return enc.Encode(out)
					}
					fmt.Printf("Agent token rotated: %s (id: %s)\n", name, rotated.ID)
					fmt.Printf("Token: %s\n", rotated.Token)
					return nil
				}
			}
			// No existing agent found — fall through to create.
		}

		agent, err := cl.CreateAgentWithOpts(name, agentCreateWithCallback)
		if err != nil {
			return fmt.Errorf("creating agent: %w", err)
		}

		if agentCreateJSON {
			out := map[string]string{
				"id":    agent.ID,
				"name":  agent.Name,
				"token": agent.Token,
			}
			if agent.CallbackSecret != "" {
				out["callback_secret"] = agent.CallbackSecret
			}
			enc := json.NewEncoder(os.Stdout)
			return enc.Encode(out)
		}

		fmt.Printf("Agent created: %s (id: %s)\n", agent.Name, agent.ID)
		fmt.Printf("Token: %s\n", agent.Token)
		if agent.CallbackSecret != "" {
			fmt.Printf("Callback secret: %s\n", agent.CallbackSecret)
		}
		return nil
	},
}

// ── agent list ──────────────────────────────────────────────────────────────

var agentListJSON bool

var agentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all agents",
	RunE: func(cmd *cobra.Command, args []string) error {
		cl, err := daemon.NewAPIClient()
		if err != nil {
			return err
		}

		agents, err := cl.GetAgents()
		if err != nil {
			return fmt.Errorf("listing agents: %w", err)
		}

		if agentListJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(agents)
		}

		if len(agents) == 0 {
			fmt.Println("No agents found.")
			return nil
		}

		fmt.Printf("%-36s  %-20s  %s\n", "ID", "NAME", "CREATED")
		for _, a := range agents {
			fmt.Printf("%-36s  %-20s  %s\n", a.ID, a.Name, a.CreatedAt.Format("2006-01-02 15:04"))
		}
		return nil
	},
}

// ── agent delete ────────────────────────────────────────────────────────────

var agentDeleteCmd = &cobra.Command{
	Use:   "delete <name-or-id>",
	Short: "Delete an agent by name or ID",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := args[0]

		cl, err := daemon.NewAPIClient()
		if err != nil {
			return err
		}

		agents, err := cl.GetAgents()
		if err != nil {
			return fmt.Errorf("listing agents: %w", err)
		}

		var agentID string
		for _, a := range agents {
			if a.ID == target || a.Name == target {
				agentID = a.ID
				break
			}
		}
		if agentID == "" {
			return fmt.Errorf("agent %q not found", target)
		}

		if err := cl.DeleteAgent(agentID); err != nil {
			return fmt.Errorf("deleting agent: %w", err)
		}
		fmt.Printf("Agent %q deleted.\n", target)
		return nil
	},
}

func init() {
	agentCreateCmd.Flags().BoolVar(&agentCreateJSON, "json", false, "Output in JSON format")
	agentCreateCmd.Flags().BoolVar(&agentCreateWithCallback, "with-callback-secret", false, "Generate and register a callback signing secret")
	agentCreateCmd.Flags().BoolVar(&agentCreateReplace, "replace", false, "Rotate token for existing agent with same name, or create if not found")

	agentListCmd.Flags().BoolVar(&agentListJSON, "json", false, "Output in JSON format")

	agentCmd.AddCommand(agentCreateCmd)
	agentCmd.AddCommand(agentListCmd)
	agentCmd.AddCommand(agentDeleteCmd)
}
