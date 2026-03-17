package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/clawvisor/clawvisor/internal/server"
)

// RunOptions controls daemon startup behavior.
type RunOptions struct {
	Foreground bool
}

// Run starts the daemon in the foreground. If no config.yaml exists in
// the daemon data directory, it runs the interactive setup wizard first.
func Run(opts RunOptions) error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}

	if err := ensureSetup(dataDir); err != nil {
		return err
	}

	// Point the server at the daemon's config file.
	os.Setenv("CONFIG_FILE", filepath.Join(dataDir, "config.yaml"))

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return server.Run(logger, server.RunOptions{})
}

// ensureDataDir resolves and creates ~/.clawvisor if needed.
func ensureDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	dataDir := filepath.Join(home, ".clawvisor")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return "", fmt.Errorf("creating data directory: %w", err)
	}
	return dataDir, nil
}

// ensureSetup checks whether config.yaml exists in dataDir. If not, it
// runs the daemon setup wizard targeting that directory.
func ensureSetup(dataDir string) error {
	cfgPath := filepath.Join(dataDir, "config.yaml")
	if _, err := os.Stat(cfgPath); err == nil {
		return nil // already configured
	}

	fmt.Println("  No config.yaml found — starting first-time setup.")
	fmt.Println()
	return runDaemonSetup(dataDir)
}

// Setup explicitly re-runs the daemon setup wizard.
func Setup() error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}
	return runDaemonSetup(dataDir)
}
