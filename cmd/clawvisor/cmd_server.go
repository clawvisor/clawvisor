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
		timingTraceDir, _ := cmd.Flags().GetString("runtime-timing-trace-dir")
		bodyTraceDir, _ := cmd.Flags().GetString("runtime-body-trace-dir")
		var timingTraceEnabled *bool
		var bodyTraceEnabled *bool
		if cmd.Flags().Changed("runtime-timing-traces") {
			v, _ := cmd.Flags().GetBool("runtime-timing-traces")
			timingTraceEnabled = &v
		}
		if cmd.Flags().Changed("runtime-body-traces") {
			v, _ := cmd.Flags().GetBool("runtime-body-traces")
			bodyTraceEnabled = &v
		}
		return server.Run(logger, server.RunOptions{
			OpenBrowser:        openBrowser,
			TimingTraceEnabled: timingTraceEnabled,
			TimingTraceDir:     timingTraceDir,
			BodyTraceEnabled:   bodyTraceEnabled,
			BodyTraceDir:       bodyTraceDir,
		})
	},
}

func init() {
	serverCmd.Flags().Bool("open", false, "Open the magic link in the default browser on startup")
	serverCmd.Flags().Bool("runtime-timing-traces", false, "Emit per-request runtime timing traces to the configured trace directory")
	serverCmd.Flags().String("runtime-timing-trace-dir", "", "Directory for verbose runtime timing trace JSONL files (overrides runtime_proxy.timing_trace_dir)")
	serverCmd.Flags().Bool("runtime-body-traces", false, "Capture request/response bodies for runtime proxy traces")
	serverCmd.Flags().String("runtime-body-trace-dir", "", "Directory for verbose runtime proxy request/response body captures (overrides runtime_proxy.body_trace_dir)")
}
