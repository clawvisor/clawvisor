package main

import (
	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/internal/daemon"
	"github.com/spf13/cobra"
)

var integrateCmd = &cobra.Command{
	Use:   "integrate",
	Short: "Set up agent integrations (Claude Code, Claude Desktop, etc.)",
	Long:  "Auto-detect installed coding agents and walk through setting up\neach one to work with Clawvisor.",
	RunE: func(cmd *cobra.Command, args []string) error {
		err := daemon.Integrate()
		if err == huh.ErrUserAborted {
			return nil
		}
		return err
	},
	SilenceUsage: true,
}

var integrateClaudeCodeCmd = &cobra.Command{
	Use:   "claude-code",
	Short: "Set up Claude Code integration",
	Long:  "Install the /clawvisor-setup slash command and optionally add\nauto-approve rules for Clawvisor curl requests.",
	RunE: func(cmd *cobra.Command, args []string) error {
		err := daemon.IntegrateClaudeCode()
		if err == huh.ErrUserAborted {
			return nil
		}
		return err
	},
	SilenceUsage: true,
}

var integrateClaudeDesktopCmd = &cobra.Command{
	Use:   "claude-desktop",
	Short: "Set up Claude Desktop integration",
	Long:  "Configure the MCP connection for Claude Desktop and optionally\nrestart it to pick up the new config.",
	RunE: func(cmd *cobra.Command, args []string) error {
		daemon.IntegrateClaudeDesktop()
		return nil
	},
	SilenceUsage: true,
}

func init() {
	integrateCmd.AddCommand(integrateClaudeCodeCmd)
	integrateCmd.AddCommand(integrateClaudeDesktopCmd)
}
