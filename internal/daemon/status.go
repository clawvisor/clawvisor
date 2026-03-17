package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Status holds the daemon's current state.
type Status struct {
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	ServerURL string `json:"server_url,omitempty"`
}

// CheckStatus reads the local session file and pings the server.
func CheckStatus() (*Status, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return &Status{}, nil
	}

	data, err := os.ReadFile(filepath.Join(home, ".clawvisor", ".local-session"))
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
		return &Status{Running: true, ServerURL: session.ServerURL, PID: os.Getpid()}, nil
	}
	return &Status{ServerURL: session.ServerURL}, nil
}

// PrintStatus prints the daemon status to stdout.
func PrintStatus(s *Status) {
	if s.Running {
		fmt.Printf("  Daemon is running at %s\n", s.ServerURL)
	} else {
		fmt.Println("  Daemon is not running.")
		if s.ServerURL != "" {
			fmt.Printf("  Last known URL: %s\n", s.ServerURL)
		}
	}
}
