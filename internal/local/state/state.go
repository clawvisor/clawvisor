package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State represents the persisted pairing state in state.json.
type State struct {
	DaemonID        string    `json:"daemon_id"`
	CloudOrigin     string    `json:"cloud_origin,omitempty"`
	ConnectionToken string    `json:"connection_token,omitempty"`
	PairedAt        time.Time `json:"paired_at,omitempty"`
}

// IsPaired returns true if a connection token is present.
func (s *State) IsPaired() bool {
	return s.ConnectionToken != ""
}

// Load reads state.json from the base directory.
// If the file doesn't exist, generates a new daemon ID and returns unpaired state.
func Load(baseDir string) (*State, error) {
	path := filepath.Join(baseDir, "state.json")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newState()
		}
		return nil, fmt.Errorf("reading state: %w", err)
	}

	// Check permissions.
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat state.json: %w", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("state.json is world-readable (mode %o); refusing to start — fix with: chmod 600 %s", info.Mode().Perm(), path)
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		// Corrupt file — try to salvage the daemon_id via a partial parse.
		id := salvageDaemonID(data)
		_ = os.Remove(path)
		if id != "" {
			return &State{DaemonID: id}, nil
		}
		return newState()
	}

	if s.DaemonID == "" {
		// Missing daemon_id — generate a new one; the spec says generated once,
		// but with no ID to preserve we have no choice.
		_ = os.Remove(path)
		return newState()
	}

	return &s, nil
}

// Save writes state.json atomically to the base directory.
func Save(baseDir string, s *State) error {
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling state: %w", err)
	}

	path := filepath.Join(baseDir, "state.json")
	tmp := path + ".tmp"

	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing temp state: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming state: %w", err)
	}

	return nil
}

// Clear removes pairing info but preserves the daemon ID.
func Clear(baseDir string, daemonID string) error {
	s := &State{DaemonID: daemonID}
	return Save(baseDir, s)
}

// salvageDaemonID attempts to extract the daemon_id from corrupt JSON
// so that a corrupted state file doesn't lose the daemon's identity.
func salvageDaemonID(data []byte) string {
	var partial map[string]json.RawMessage
	if err := json.Unmarshal(data, &partial); err != nil {
		return ""
	}
	raw, ok := partial["daemon_id"]
	if !ok {
		return ""
	}
	var id string
	if json.Unmarshal(raw, &id) != nil {
		return ""
	}
	return id
}

func newState() (*State, error) {
	id, err := generateDaemonID()
	if err != nil {
		return nil, fmt.Errorf("generating daemon ID: %w", err)
	}
	return &State{DaemonID: id}, nil
}

func generateDaemonID() (string, error) {
	b := make([]byte, 4) // 4 bytes = 8 hex chars
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenerateConnectionToken generates a 256-bit random connection token.
func GenerateConnectionToken() (string, error) {
	b := make([]byte, 32) // 32 bytes = 64 hex chars
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
