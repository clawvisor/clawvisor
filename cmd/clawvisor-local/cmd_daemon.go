package main

import "github.com/spf13/cobra"

// daemonParentCmd bundles the daemon-as-OS-service lifecycle commands
// under a `daemon` parent. Used to install / uninstall / start / stop /
// restart the launchd-or-systemd unit that supervises clawvisor-local
// itself across user logins. Distinct from `services …` (which manage
// the *plugin* services this daemon supervises in turn).
func daemonParentCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "daemon",
		Short: "Manage clawvisor-local as a launchd / systemd service",
		Long: `These subcommands install clawvisor-local itself as a system
service so it starts at login and restarts on crash. Distinct from
the "services" subcommands, which install plugin services that the
daemon then supervises.`,
	}
	parent.AddCommand(
		installServiceCmd(),
		uninstallServiceCmd(),
		startServiceCmd(),
		stopServiceCmd(),
		restartServiceCmd(),
	)
	return parent
}
