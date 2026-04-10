package version

import (
	"context"
	"log/slog"
	"os"
	"syscall"
	"time"
)

// StartAutoUpdater runs a background goroutine that periodically checks for
// new releases and applies them. After a successful update, the process is
// gracefully restarted by sending itself SIGTERM (which the server's signal
// handler catches for graceful shutdown) followed by an exec of the new binary.
//
// The goroutine exits when ctx is cancelled. It is a no-op for dev builds.
func StartAutoUpdater(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if v := GetCurrent(); v == "dev" || v == "" {
		logger.Debug("auto-update: skipping for dev build")
		return
	}

	logger.Info("auto-update: enabled", "check_interval", interval)

	go func() {
		// Stagger the first check so it doesn't fire immediately on startup.
		firstCheck := interval
		if firstCheck > 5*time.Minute {
			firstCheck = 5 * time.Minute
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(firstCheck):
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			if applied := checkAndApply(logger); applied {
				return // Binary replaced, SIGTERM sent — stop the goroutine.
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// checkAndApply checks for a newer version and applies it. Returns true if
// an update was successfully applied and SIGTERM was sent.
func checkAndApply(logger *slog.Logger) bool {
	info := Check()
	if !info.UpdateAvail || info.Latest == "" {
		logger.Debug("auto-update: no update available", "current", info.Current, "latest", info.Latest)
		return false
	}

	logger.Info("auto-update: new version available, applying",
		"current", info.Current, "target", info.Latest)

	oldVer, newVer, err := Apply(info.Latest)
	if err != nil {
		logger.Error("auto-update: failed to apply update", "error", err)
		return false
	}

	logger.Info("auto-update: binary replaced, restarting",
		"old_version", oldVer, "new_version", newVer)

	// Update the in-memory version under the cache lock so any concurrent
	// Check() / GetCurrent() calls see the new value without a data race.
	cacheMu.Lock()
	Version = newVer
	cachedInfo = nil
	cacheMu.Unlock()

	// Send SIGTERM to self for graceful shutdown. The process manager
	// (systemd, Docker, etc.) will restart with the new binary.
	//
	// We send SIGTERM rather than calling os.Exit directly so the server's
	// existing signal handler can drain connections gracefully.
	p, err := os.FindProcess(os.Getpid())
	if err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
	return true
}
