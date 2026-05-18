package clawvisorcli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/pkg/version"
)

var rootCmd = &cobra.Command{
	Use:     "clawvisor-server",
	Short:   "Clawvisor — AI Gatekeeper",
	Long:    "Clawvisor is a gatekeeper service for AI agents. Manage your server, dashboard, and setup from a single binary.",
	Version: version.Version,
}

func init() {
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
	rootCmd.AddCommand(autoUpdateCmd)
	rootCmd.AddCommand(healthcheckCmd)
	rootCmd.AddCommand(agentCmd)
	rootCmd.AddCommand(servicesCmd)
	rootCmd.AddCommand(connectAgentCmd)
	rootCmd.AddCommand(serverCmd)
	rootCmd.AddCommand(tuiCmd)
	rootCmd.AddCommand(validateCmd)
}

// Execute runs the Clawvisor server CLI. The command name follows argv[0] so
// the legacy clawvisor wrapper and the clawvisor-server binary share behavior.
func Execute() error {
	name := filepath.Base(os.Args[0])
	if name == "" {
		name = "clawvisor-server"
	}
	rootCmd.Use = name
	rootCmd.SetVersionTemplate(fmt.Sprintf("%s %s\n", name, version.Version))
	return rootCmd.Execute()
}
