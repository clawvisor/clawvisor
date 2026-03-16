package main

import (
	"log/slog"
	"os"

	"github.com/clawvisor/clawvisor/internal/server"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := server.Run(logger, server.RunOptions{}); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}
