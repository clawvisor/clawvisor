package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/clawvisor"
	"github.com/clawvisor/clawvisor/pkg/store"
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
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

		_, st, err := clawvisor.ConnectStore(logger)
		if err != nil {
			return fmt.Errorf("connecting to database: %w", err)
		}
		defer st.Close()

		ctx := context.Background()
		user, err := ensureAdminUser(ctx, st)
		if err != nil {
			return err
		}

		// If --replace, delete any existing agent with the same name first.
		if agentCreateReplace {
			existing, _ := st.ListAgents(ctx, user.ID)
			for _, a := range existing {
				if a.Name == name {
					_ = st.DeleteAgent(ctx, a.ID, user.ID)
				}
			}
		}

		rawToken, err := auth.GenerateAgentToken()
		if err != nil {
			return fmt.Errorf("generating token: %w", err)
		}

		agent, err := st.CreateAgent(ctx, user.ID, name, auth.HashToken(rawToken))
		if err != nil {
			return fmt.Errorf("creating agent: %w", err)
		}

		var callbackSecret string
		if agentCreateWithCallback {
			secret, err := auth.GenerateCallbackSecret()
			if err != nil {
				return fmt.Errorf("generating callback secret: %w", err)
			}
			if err := st.SetAgentCallbackSecret(ctx, agent.ID, secret); err != nil {
				return fmt.Errorf("storing callback secret: %w", err)
			}
			callbackSecret = secret
		}

		if agentCreateJSON {
			out := map[string]string{
				"id":    agent.ID,
				"name":  agent.Name,
				"token": rawToken,
			}
			if callbackSecret != "" {
				out["callback_secret"] = callbackSecret
			}
			enc := json.NewEncoder(os.Stdout)
			return enc.Encode(out)
		}

		fmt.Printf("Agent created: %s (id: %s)\n", agent.Name, agent.ID)
		fmt.Printf("Token: %s\n", rawToken)
		if callbackSecret != "" {
			fmt.Printf("Callback secret: %s\n", callbackSecret)
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
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

		_, st, err := clawvisor.ConnectStore(logger)
		if err != nil {
			return fmt.Errorf("connecting to database: %w", err)
		}
		defer st.Close()

		ctx := context.Background()
		user, err := ensureAdminUser(ctx, st)
		if err != nil {
			return err
		}

		agents, err := st.ListAgents(ctx, user.ID)
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
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

		_, st, err := clawvisor.ConnectStore(logger)
		if err != nil {
			return fmt.Errorf("connecting to database: %w", err)
		}
		defer st.Close()

		ctx := context.Background()
		user, err := ensureAdminUser(ctx, st)
		if err != nil {
			return err
		}

		agents, err := st.ListAgents(ctx, user.ID)
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

		if err := st.DeleteAgent(ctx, agentID, user.ID); err != nil {
			return fmt.Errorf("deleting agent: %w", err)
		}
		fmt.Printf("Agent %q deleted.\n", target)
		return nil
	},
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// ensureAdminUser finds admin@local, creating it if missing (same pattern as
// internal/server/run.go).
func ensureAdminUser(ctx context.Context, st store.Store) (*store.User, error) {
	const localEmail = "admin@local"

	user, err := st.GetUserByEmail(ctx, localEmail)
	if err == nil {
		return user, nil
	}

	// Create the admin user with a random password.
	randPw := make([]byte, 32)
	if _, err := cryptorand.Read(randPw); err != nil {
		return nil, fmt.Errorf("generating random password: %w", err)
	}
	hash, err := auth.HashPassword(hex.EncodeToString(randPw))
	if err != nil {
		return nil, fmt.Errorf("hashing password: %w", err)
	}
	if _, err := st.CreateUser(ctx, localEmail, hash); err != nil {
		return nil, fmt.Errorf("creating admin user: %w", err)
	}

	return st.GetUserByEmail(ctx, localEmail)
}

func init() {
	agentCreateCmd.Flags().BoolVar(&agentCreateJSON, "json", false, "Output in JSON format")
	agentCreateCmd.Flags().BoolVar(&agentCreateWithCallback, "with-callback-secret", false, "Generate and register a callback signing secret")
	agentCreateCmd.Flags().BoolVar(&agentCreateReplace, "replace", false, "Delete existing agent with same name before creating")

	agentListCmd.Flags().BoolVar(&agentListJSON, "json", false, "Output in JSON format")

	agentCmd.AddCommand(agentCreateCmd)
	agentCmd.AddCommand(agentListCmd)
	agentCmd.AddCommand(agentDeleteCmd)
}
