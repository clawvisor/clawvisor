package main

import (
	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/internal/daemon"
	"github.com/spf13/cobra"
)

var servicesCmd = &cobra.Command{
	Use:   "services",
	Short: "Manage service connections (GitHub, Gmail, Slack, etc.)",
	Long:  "Connect, list, and manage the external services that Clawvisor\nproxies on behalf of your agents. Requires a running daemon.",
	RunE: func(cmd *cobra.Command, args []string) error {
		err := daemon.Services()
		if err == huh.ErrUserAborted {
			return nil
		}
		return err
	},
	SilenceUsage: true,
}

var servicesListJSON bool

var servicesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available and connected services",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.ServicesList(servicesListJSON)
	},
	SilenceUsage: true,
}

var servicesAddCmd = &cobra.Command{
	Use:   "add [service]",
	Short: "Connect a service",
	Long:  "Connect a service by ID or name. If no service is specified,\nan interactive picker is shown.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		serviceID := ""
		if len(args) > 0 {
			serviceID = args[0]
		}
		err := daemon.ServicesAdd(serviceID)
		if err == huh.ErrUserAborted {
			return nil
		}
		return err
	},
	SilenceUsage: true,
}

var servicesRemoveCmd = &cobra.Command{
	Use:   "remove <service>",
	Short: "Disconnect a service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.ServicesRemove(args[0])
	},
	SilenceUsage: true,
}

func init() {
	servicesListCmd.Flags().BoolVar(&servicesListJSON, "json", false, "Output in JSON format")

	servicesCmd.AddCommand(servicesListCmd)
	servicesCmd.AddCommand(servicesAddCmd)
	servicesCmd.AddCommand(servicesRemoveCmd)
}
