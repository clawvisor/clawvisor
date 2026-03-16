package main

import (
	"log/slog"
	"os"

	"github.com/clawvisor/clawvisor/internal/server"
	"github.com/spf13/cobra"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the Clawvisor API server",
	RunE: func(cmd *cobra.Command, args []string) error {
		logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
		openBrowser, _ := cmd.Flags().GetBool("open")
		return server.Run(logger, server.RunOptions{OpenBrowser: openBrowser})
	},
}

func init() {
	serverCmd.Flags().Bool("open", false, "Open the magic link in the default browser on startup")
}
