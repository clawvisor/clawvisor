package main

import (
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "clawvisor",
	Short: "Clawvisor — AI Gatekeeper",
	Long:  "Clawvisor is a gatekeeper service for AI agents. Manage your server, dashboard, and setup from a single binary.",
}

func init() {
	rootCmd.AddCommand(serverCmd)
	rootCmd.AddCommand(tuiCmd)
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(agentCmd)
	rootCmd.AddCommand(healthcheckCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
