package main

import (
	"fmt"

	"github.com/clawvisor/clawvisor/internal/daemon"
	"github.com/spf13/cobra"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run Clawvisor as a background daemon",
	Long:  "Run, install, or check the status of the Clawvisor daemon.\nWith no subcommand, starts the daemon in the foreground.",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.Run(daemon.RunOptions{Foreground: true})
	},
}

var daemonRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the daemon in the foreground",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.Run(daemon.RunOptions{Foreground: true})
	},
}

var daemonInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the daemon as a system service (launchd on macOS, systemd on Linux)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.Install()
	},
}

var daemonUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the daemon system service",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.Uninstall()
	},
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the installed daemon service",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.Start()
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon service",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.Stop()
	},
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := daemon.CheckStatus()
		if err != nil {
			return fmt.Errorf("checking status: %w", err)
		}
		daemon.PrintStatus(s)
		return nil
	},
}

var daemonSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Re-run the first-run setup wizard",
	RunE: func(cmd *cobra.Command, args []string) error {
		pair, _ := cmd.Flags().GetBool("pair")
		return daemon.Setup(daemon.SetupOptions{Pair: pair})
	},
}

var daemonPairCmd = &cobra.Command{
	Use:   "pair",
	Short: "Pair a mobile device via QR code",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.Pair()
	},
}

var daemonDashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Open the daemon dashboard in your browser",
	RunE: func(cmd *cobra.Command, args []string) error {
		noOpen, _ := cmd.Flags().GetBool("no-open")
		return daemon.Dashboard(noOpen)
	},
}

func init() {
	daemonSetupCmd.Flags().Bool("pair", false, "Pair a mobile device after setup and print the agent setup URL")
	daemonDashboardCmd.Flags().Bool("no-open", false, "Print the URL instead of opening the browser")

	daemonCmd.AddCommand(daemonRunCmd)
	daemonCmd.AddCommand(daemonInstallCmd)
	daemonCmd.AddCommand(daemonUninstallCmd)
	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonSetupCmd)
	daemonCmd.AddCommand(daemonPairCmd)
	daemonCmd.AddCommand(daemonDashboardCmd)
}
