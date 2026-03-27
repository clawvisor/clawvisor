package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/pkg/version"
)

var rootCmd = &cobra.Command{
	Use:     "clawvisor",
	Short:   "Clawvisor — AI Gatekeeper",
	Long:    "Clawvisor is a gatekeeper service for AI agents. Manage your server, dashboard, and setup from a single binary.",
	Version: version.Version,
}

func init() {
	rootCmd.SetVersionTemplate(fmt.Sprintf("clawvisor %s\n", version.Version))
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(restartCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(dashboardCmd)
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(pairCmd)
	rootCmd.AddCommand(installCmd)
	rootCmd.AddCommand(uninstallCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(healthcheckCmd)
	rootCmd.AddCommand(agentCmd)
	rootCmd.AddCommand(servicesCmd)
	rootCmd.AddCommand(integrateCmd)
	rootCmd.AddCommand(serverCmd)
	rootCmd.AddCommand(tuiCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
