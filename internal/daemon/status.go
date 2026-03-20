package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/clawvisor/clawvisor/internal/browser"
)

// Status holds the daemon's current state.
type Status struct {
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	ServerURL string `json:"server_url,omitempty"`
	DaemonID  string `json:"daemon_id,omitempty"`
}

// CheckStatus reads the local session file and pings the server.
func CheckStatus() (*Status, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return &Status{}, nil
	}

	dataDir := filepath.Join(home, ".clawvisor")

	data, err := os.ReadFile(filepath.Join(dataDir, ".local-session"))
	if err != nil {
		return &Status{}, nil
	}

	var session struct {
		ServerURL string `json:"server_url"`
	}
	if err := json.Unmarshal(data, &session); err != nil || session.ServerURL == "" {
		return &Status{}, nil
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(session.ServerURL + "/health")
	if err != nil {
		return &Status{ServerURL: session.ServerURL}, nil
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		pid := readPIDFile(filepath.Join(dataDir, ".daemon.pid"))
		daemonID, _ := readDaemonID(filepath.Join(dataDir, "config.yaml"))
		return &Status{Running: true, ServerURL: session.ServerURL, PID: pid, DaemonID: daemonID}, nil
	}
	return &Status{ServerURL: session.ServerURL}, nil
}

// Dashboard constructs a magic-link URL for the running daemon and opens it
// in the default browser. If noOpen is true, it prints the URL instead.
func Dashboard(noOpen bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	dataDir := filepath.Join(home, ".clawvisor")
	serverURL, magicToken, err := readLocalSession(dataDir)
	if err != nil {
		return fmt.Errorf("no local session found — is the daemon running?")
	}
	if serverURL == "" || magicToken == "" {
		return fmt.Errorf("incomplete local session — try restarting the daemon")
	}

	dashURL := fmt.Sprintf("%s/magic-link?token=%s", serverURL, magicToken)

	if noOpen {
		fmt.Println(dashURL)
		return nil
	}

	if !browser.Open(dashURL) {
		fmt.Println("  Could not open browser. Visit this URL:")
		fmt.Println("  " + dashURL)
		return nil
	}

	fmt.Println("  Opening dashboard in browser...")
	return nil
}

// PrintStatus prints the daemon status to stdout.
func PrintStatus(s *Status) {
	if s.Running {
		if s.PID > 0 {
			fmt.Printf("  Daemon is running at %s (PID %d)\n", s.ServerURL, s.PID)
		} else {
			fmt.Printf("  Daemon is running at %s\n", s.ServerURL)
		}
		if s.DaemonID != "" {
			fmt.Printf("  Daemon ID: %s\n", s.DaemonID)
		}
	} else {
		fmt.Println("  Daemon is not running.")
		if s.ServerURL != "" {
			fmt.Printf("  Last known URL: %s\n", s.ServerURL)
		}
	}
}
