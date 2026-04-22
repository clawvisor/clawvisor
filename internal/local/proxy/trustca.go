package proxy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// TrustCA installs the proxy's CA certificate into the system trust
// store so HTTPS clients accept the MITM interception without errors.
// Runs from whatever process calls it — the CLI invokes it directly
// (terminal-visible keychain prompt), the daemon invokes it from an
// HTTP handler (macOS pops the prompt system-wide; Linux needs sudo).
//
// Platform behaviors:
//
//	darwin — `security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db`
//	         The OS prompts for the user's keychain password regardless
//	         of the calling process.
//
//	linux  — `sudo sh -c "cp … /usr/local/share/ca-certificates/
//	         clawvisor-proxy.crt && update-ca-certificates"`. Prompts
//	         for sudo in whatever terminal the calling process inherits.
//	         Not usable from a GUI-invoked daemon without askpass.
//
// Returns a user-facing error message on failure. caPath must exist.
func TrustCA(caPath string) error {
	if _, err := os.Stat(caPath); err != nil {
		return fmt.Errorf("CA cert not found at %s (has the proxy run at least once?): %w", caPath, err)
	}

	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		keychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
		c := exec.Command("security", "add-trusted-cert",
			"-r", "trustRoot",
			"-k", keychain,
			caPath,
		)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("security add-trusted-cert failed: %w", err)
		}
		return nil

	case "linux":
		dst := "/usr/local/share/ca-certificates/clawvisor-proxy.crt"
		c := exec.Command("sudo", "sh", "-c",
			fmt.Sprintf("cp %q %q && update-ca-certificates", caPath, dst))
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("install CA: %w", err)
		}
		return nil

	default:
		return fmt.Errorf("trust-ca not implemented for %s — install %s manually", runtime.GOOS, caPath)
	}
}
