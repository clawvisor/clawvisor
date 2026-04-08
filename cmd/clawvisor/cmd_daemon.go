package main

import (
	"fmt"

	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/internal/daemon"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Clawvisor daemon",
	Long:  "Start the Clawvisor daemon as a background service.\nUse --foreground to run in the foreground instead.",
	RunE: func(cmd *cobra.Command, args []string) error {
		fg, _ := cmd.Flags().GetBool("foreground")
		if fg {
			return daemon.Run(daemon.RunOptions{Foreground: true})
		}
		return daemon.Start()
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.Stop()
	},
}

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the daemon service",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := daemon.Stop(); err != nil {
			return err
		}
		return daemon.Start()
	},
}

var statusCmd = &cobra.Command{
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

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Guided setup: configure, install, start, connect services and agents",
	Long:  "Run the full guided setup flow: configure the daemon, install it as\na system service, start it, connect services, and set up agent\nintegrations. This is the recommended way to get started.",
	RunE: func(cmd *cobra.Command, args []string) error {
		pair, _ := cmd.Flags().GetBool("pair")
		err := daemon.Install(daemon.InstallOptions{Pair: pair})
		if err == huh.ErrUserAborted {
			return nil
		}
		return err
	},
	SilenceUsage: true,
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the daemon system service",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.Uninstall()
	},
}

var pairCmd = &cobra.Command{
	Use:   "pair",
	Short: "Pair a mobile device via QR code",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.Pair()
	},
}

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Open the Clawvisor dashboard in your browser",
	RunE: func(cmd *cobra.Command, args []string) error {
		noOpen, _ := cmd.Flags().GetBool("no-open")
		return daemon.Dashboard(noOpen)
	},
}

func init() {
	startCmd.Flags().Bool("foreground", false, "Run in the foreground instead of starting the background service")
	installCmd.Flags().Bool("pair", false, "Pair a mobile device after install and print the agent setup URL")
	dashboardCmd.Flags().Bool("no-open", false, "Print the URL instead of opening the browser")
}
