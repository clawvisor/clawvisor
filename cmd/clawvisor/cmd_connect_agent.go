package main

import (
	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/internal/daemon"
	"github.com/spf13/cobra"
)

var connectAgentCmd = &cobra.Command{
	Use:     "connect-agent",
	Aliases: []string{"integrate"},
	Short:   "Connect an agent to Clawvisor (Claude Code, Claude Desktop, etc.)",
	Long:    "Auto-detect installed coding agents and walk through setting up\neach one to work with Clawvisor.",
	RunE: func(cmd *cobra.Command, args []string) error {
		err := daemon.ConnectAgent()
		if err == huh.ErrUserAborted {
			return nil
		}
		return err
	},
	SilenceUsage: true,
}

var connectAgentClaudeCodeCmd = &cobra.Command{
	Use:   "claude-code",
	Short: "Connect Claude Code to Clawvisor",
	Long:  "Install the /clawvisor-setup slash command and optionally add\nauto-approve rules for Clawvisor curl requests.",
	RunE: func(cmd *cobra.Command, args []string) error {
		err := daemon.ConnectAgentClaudeCode()
		if err == huh.ErrUserAborted {
			return nil
		}
		return err
	},
	SilenceUsage: true,
}

var connectAgentClaudeDesktopCmd = &cobra.Command{
	Use:   "claude-desktop",
	Short: "Connect Claude Desktop to Clawvisor",
	Long:  "Configure the MCP connection for Claude Desktop and optionally\nrestart it to pick up the new config.",
	RunE: func(cmd *cobra.Command, args []string) error {
		daemon.ConnectAgentClaudeDesktop()
		return nil
	},
	SilenceUsage: true,
}

func init() {
	connectAgentCmd.AddCommand(connectAgentClaudeCodeCmd)
	connectAgentCmd.AddCommand(connectAgentClaudeDesktopCmd)
}
