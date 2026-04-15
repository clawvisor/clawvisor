//go:build !darwin

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func toolbarCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "toolbar",
		Short: "Run the local daemon as a menu bar app (macOS only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("toolbar mode is only supported on macOS")
		},
	}
}
