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
func (d *Daemon) proxyEndpoints() pairing.ProxyEndpoints {
	return pairing.ProxyEndpoints{
		Status:    d.handleProxyStatus,
		Configure: d.handleProxyConfigure,
		Enable:    d.handleProxyEnable,
		Disable:   d.handleProxyDisable,
		Restart:   d.handleProxyRestart,
		SetMode:   d.handleProxySetMode,
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
	if err := d.proxy.Configure(cfg, req.ProxyToken); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.AutoEnable {
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
