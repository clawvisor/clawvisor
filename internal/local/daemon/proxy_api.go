package daemon

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/local/pairing"
	"github.com/clawvisor/clawvisor/internal/local/proxy"
)

// proxyEndpoints returns the HTTP handlers mounted on the pairing
// server's /api/proxy/* paths. The pairing server itself knows nothing
// about the proxy type — it just invokes these callbacks.
//
// Wire contract:
//
//	GET  /api/proxy/status     → proxy.Status JSON
//	POST /api/proxy/configure  body: ConfigureRequest
//	POST /api/proxy/enable     body: (empty or {}): enables currently-configured proxy
//	POST /api/proxy/disable    body: (empty)
//	POST /api/proxy/restart    body: (empty)
//	POST /api/proxy/set-mode   body: {"mode": "observe" | "enforce"}
//	POST /api/proxy/trust-ca   body: (empty). Invokes the OS trust-store
//	                           installer on the CA cert the daemon wrote
//	                           during its first proxy start. On macOS the
//	                           keychain-password modal pops system-wide.
//	POST /api/proxy/install-binary body: {"server_url": "..."} (optional;
//	                           defaults to the daemon's configured
//	                           server_url). Pulls the platform-appropriate
//	                           binary from the server's /api/proxy/download
//	                           endpoint and writes it to the conventional
//	                           install path. Leaves lifecycle untouched —
//	                           follow with a Configure call if you want
//	                           the daemon to pick up the new binary.
func (d *Daemon) proxyEndpoints() pairing.ProxyEndpoints {
	return pairing.ProxyEndpoints{
		Status:        d.handleProxyStatus,
		Configure:     d.handleProxyConfigure,
		Enable:        d.handleProxyEnable,
		Disable:       d.handleProxyDisable,
		Restart:       d.handleProxyRestart,
		SetMode:       d.handleProxySetMode,
		TrustCA:       d.handleProxyTrustCA,
		InstallBinary: d.handleProxyInstallBinary,
	}
}

type proxyConfigureRequest struct {
	BinaryPath string `json:"binary_path"`
	ServerURL  string `json:"server_url"`
	ProxyToken string `json:"proxy_token"`
	BridgeID   string `json:"bridge_id"`
	ListenHost string `json:"listen_host"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode"`
	// AutoEnable starts the proxy after a successful configure.
	// Convenient for "install + start" in one CLI call.
	AutoEnable bool `json:"auto_enable"`
}

func (d *Daemon) handleProxyStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, d.proxy.Status())
}

func (d *Daemon) handleProxyConfigure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req proxyConfigureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cfg := proxy.Config{
		BinaryPath: req.BinaryPath,
		ServerURL:  req.ServerURL,
		BridgeID:   req.BridgeID,
		ListenHost: req.ListenHost,
		ListenPort: req.ListenPort,
		Mode:       req.Mode,
	}
	// Capture pre-configure state so we know whether to auto-restart
	// after persistence. Without this, reconfiguring a live proxy
	// (token rotation, mode change, server-url move) would silently
	// keep the old config in the running process.
	wasRunning := d.proxy.Status().State == proxy.StateRunning

	if err := d.proxy.Configure(cfg, req.ProxyToken); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	switch {
	case req.AutoEnable && wasRunning:
		// Already running with old config — restart to pick up the new one.
		if err := d.proxy.Restart(); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":            true,
				"restart_error": err.Error(),
				"status":        d.proxy.Status(),
			})
			return
		}
	case req.AutoEnable:
		if err := d.proxy.Enable(); err != nil {
			// Configure succeeded; surface the Enable error separately
			// so the caller can diagnose start-time issues (missing
			// binary, port conflict, etc.) vs. config-time issues.
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":           true,
				"enable_error": err.Error(),
				"status":       d.proxy.Status(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": d.proxy.Status()})
}

func (d *Daemon) handleProxyEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := d.proxy.Enable(); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": d.proxy.Status()})
}

func (d *Daemon) handleProxyDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := d.proxy.Disable(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": d.proxy.Status()})
}

func (d *Daemon) handleProxyRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := d.proxy.Restart(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": d.proxy.Status()})
}

type proxySetModeRequest struct {
	Mode string `json:"mode"`
}

func (d *Daemon) handleProxySetMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req proxySetModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cur := d.proxy.Status()
	if !cur.Enabled {
		writeErr(w, http.StatusConflict, "proxy is not enabled")
		return
	}
	// Re-use Configure so validation stays in one place; we need to
	// look up the existing binary + bridge from the manager's current
	// status since the caller should only need to flip the mode.
	cfg := proxy.Config{
		BinaryPath: cur.BinaryPath,
		ServerURL:  cur.ServerURL,
		BridgeID:   cur.BridgeID,
		ListenHost: cur.ListenHost,
		ListenPort: cur.ListenPort,
		Mode:       req.Mode,
	}
	if err := d.proxy.Configure(cfg, ""); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.proxy.Restart(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": d.proxy.Status()})
}

// handleProxyTrustCA invokes the OS trust-store installer on the proxy's
// CA cert. On macOS this pops the keychain-password modal from the
// daemon's process — because launchd child processes share the user's
// graphical session, the prompt appears system-wide. On Linux this
// shells out to sudo and is only viable from a terminal-attached
// daemon (not the typical dashboard-driven flow).
//
// Returns quickly on the happy path. A non-2xx response means either
// (a) the CA cert is missing (proxy has never run) or (b) the user
// cancelled the prompt / sudo failed.
func (d *Daemon) handleProxyTrustCA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	caPath := d.proxy.CACertPath()
	if err := proxy.TrustCA(caPath); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ca_cert_path": caPath})
}

type proxyInstallBinaryRequest struct {
	// ServerURL overrides the daemon's persisted server_url. Empty
	// falls back to whatever /api/proxy/configure last received (or
	// the currently-enabled proxy's config).
	ServerURL string `json:"server_url,omitempty"`
}

// handleProxyInstallBinary pulls the platform-appropriate proxy binary
// from the configured Clawvisor server's /api/proxy/download endpoint
// and writes it to the conventional install path. Does NOT touch the
// running proxy — the caller issues a Configure (or Restart) next if
// they want the daemon to pick up the new binary.
//
// The dashboard one-click flow uses this during initial connect so
// users don't have to manually `brew install` or build from source.
func (d *Daemon) handleProxyInstallBinary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req proxyInstallBinaryRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	serverURL := req.ServerURL
	if serverURL == "" {
		serverURL = d.proxy.Status().ServerURL
	}
	if serverURL == "" {
		writeErr(w, http.StatusBadRequest,
			"no server_url — pass one in the body or run Configure first")
		return
	}
	dst := proxy.DefaultBinaryPath()
	if err := proxy.DownloadBinaryFromServer(serverURL, dst); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "binary_path": dst})
}

// -- helpers -----------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// reserved for test shims
var _ = errors.New
