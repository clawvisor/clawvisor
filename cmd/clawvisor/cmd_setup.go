package main

import (
	"github.com/clawvisor/clawvisor/internal/daemon"
	"github.com/spf13/cobra"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Run the first-time setup wizard",
	RunE: func(cmd *cobra.Command, args []string) error {
		pair, _ := cmd.Flags().GetBool("pair")
		return daemon.Setup(daemon.SetupOptions{Pair: pair})
	},
}

func init() {
	setupCmd.Flags().Bool("pair", false, "Pair a mobile device after setup and print the agent setup URL")
}
