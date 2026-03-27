package main

import (
	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/internal/daemon"
	"github.com/spf13/cobra"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure the daemon (LLM, relay, telemetry)",
	Long:  "Run the core configuration wizard to set up LLM provider, relay,\nand telemetry preferences. Use `clawvisor services` and\n`clawvisor integrate` to connect services and agents.",
	RunE: func(cmd *cobra.Command, args []string) error {
		err := daemon.Setup()
		if err == huh.ErrUserAborted {
			return nil
		}
		return err
	},
	SilenceUsage: true,
}
