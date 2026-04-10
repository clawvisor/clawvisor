package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/internal/daemon"
	"github.com/clawvisor/clawvisor/pkg/version"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update Clawvisor to the latest release",
	RunE:  runUpdate,
}

func init() {
	updateCmd.Flags().String("version", "", "Update to a specific version instead of latest")
}

func runUpdate(cmd *cobra.Command, args []string) error {
	pinned, _ := cmd.Flags().GetString("version")

	// Determine target version.
	var targetVersion string
	if pinned != "" {
		targetVersion = strings.TrimPrefix(pinned, "v")
	} else {
		fmt.Println("  Checking for updates...")
		info := version.Check()
		if info.Latest == "" {
			return fmt.Errorf("could not determine latest version")
		}
		if !info.UpdateAvail {
			fmt.Printf("  Already up to date (v%s).\n", info.Current)
			return nil
		}
		targetVersion = info.Latest
	}

	current := version.GetCurrent()
	fmt.Printf("  Updating v%s → v%s\n", current, targetVersion)

	oldVer, newVer, err := version.Apply(targetVersion)
	if err != nil {
		return err
	}

	fmt.Printf("  Updated successfully: v%s → v%s\n", oldVer, newVer)

	// Restart daemon automatically if it was running.
	if s, err := daemon.CheckStatus(); err == nil && s.Running {
		fmt.Println("  Restarting daemon...")
		if err := daemon.Stop(); err != nil {
			return fmt.Errorf("stopping daemon: %w", err)
		}
		if err := daemon.Start(); err != nil {
			return fmt.Errorf("starting daemon: %w", err)
		}
		fmt.Println("  Daemon restarted.")
	}

	return nil
}
