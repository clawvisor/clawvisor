// Package proxy is the first-class Clawvisor Network Proxy lifecycle
// manager, owned directly by the clawvisor-local daemon. Unlike the
// pluggable service model (internal/local/services) — which bridges
// local capabilities outward to remote agents — the proxy is a
// deeply-coupled Clawvisor subsystem that intercepts agent traffic
// inward. It deserves its own lifecycle, config, and API surface
// instead of being squeezed through the generic service shape.
//
// State machine:
//
//	Disabled ──(Enable)──▶ Starting ──▶ Running ──(Disable)──▶ Stopped
//	                           │              │
//	                           └─(fail)──▶ Failed ◀─(crash)──┘
//
// The manager owns exactly one proxy process. Enable/Disable/Restart
// are serialized through a mutex; Status is lock-free (atomic read).
package proxy

import (
	"time"
)

// Config is the persisted proxy configuration. Written to
// ~/.clawvisor/proxy/config.json on Configure; read on daemon boot.
// ProxyToken is stored separately (current: plain file with 0600
// perms; future: keychain) so the config JSON can be safely dumped in
// diagnostics.
type Config struct {
	Enabled    bool   `json:"enabled"`
	ListenHost string `json:"listen_host"`
	ListenPort int    `json:"listen_port"`
	BridgeID   string `json:"bridge_id"`
	ServerURL  string `json:"server_url"`
	Mode       string `json:"mode"` // "observe" | "enforce"
	BinaryPath string `json:"binary_path"`
}

// Defaults returns a Config with the stable defaults for a first-run
// daemon — port 25298 (Clawvisor family block), loopback only, observe
// mode so enforcement is an explicit second step.
func Defaults() Config {
	return Config{
		ListenHost: "127.0.0.1",
		ListenPort: 25298,
		ServerURL:  "http://127.0.0.1:25297",
		Mode:       "observe",
	}
}

// State is the public-facing lifecycle state.
type State string

const (
	StateDisabled State = "disabled"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateFailed   State = "failed"
	StateStopped  State = "stopped"
)

// Status is the snapshot returned by Manager.Status() — safe to JSON-
// encode for the daemon's status endpoint.
type Status struct {
	State       State     `json:"state"`
	Enabled     bool      `json:"enabled"`
	ListenHost  string    `json:"listen_host,omitempty"`
	ListenPort  int       `json:"listen_port,omitempty"`
	BridgeID    string    `json:"bridge_id,omitempty"`
	ServerURL   string    `json:"server_url,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	PID         int       `json:"pid,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	RestartCount int      `json:"restart_count"`
	LastError   string    `json:"last_error,omitempty"`
	BinaryPath  string    `json:"binary_path,omitempty"`
	CACertPath  string    `json:"ca_cert_path,omitempty"`
}
