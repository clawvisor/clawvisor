package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/internal/daemon"
)

var autoUpdateCmd = &cobra.Command{
	Use:   "auto-update",
	Short: "Manage automatic binary updates",
	Long:  "Enable, disable, or check the status of automatic binary updates.\nWhen enabled, Clawvisor periodically checks GitHub for new releases\nand applies them automatically.",
}

var autoUpdateEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable automatic updates",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := daemon.SetAutoUpdate(true); err != nil {
			return err
		}
		fmt.Println("  Auto-update enabled. Restart the daemon for changes to take effect.")
		return nil
	},
}

var autoUpdateDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable automatic updates",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := daemon.SetAutoUpdate(false); err != nil {
			return err
		}
		fmt.Println("  Auto-update disabled.")
		return nil
	},
}

var autoUpdateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show auto-update configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		enabled, interval, err := daemon.AutoUpdateStatus()
		if err != nil {
			return err
		}
		if enabled {
			fmt.Printf("  Auto-update: enabled (check interval: %s)\n", interval)
		} else {
			fmt.Println("  Auto-update: disabled")
			fmt.Println("  Run `clawvisor auto-update enable` to opt in.")
		}
		return nil
	},
}

func init() {
	autoUpdateCmd.AddCommand(autoUpdateEnableCmd)
	autoUpdateCmd.AddCommand(autoUpdateDisableCmd)
	autoUpdateCmd.AddCommand(autoUpdateStatusCmd)
}
