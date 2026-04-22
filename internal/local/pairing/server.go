package pairing

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// ProxyController is the interface the pairing server uses to drive
// the Clawvisor Network Proxy lifecycle (see internal/local/proxy).
// Injected by the daemon so this package doesn't import proxy directly.
type ProxyController interface {
	Configure(cfg interface{}, token string) error
	Enable() error
	Disable() error
	Restart() error
	Status() interface{}
}

// Server is the localhost HTTP server for pairing and status.
type Server struct {
	port           int
	daemonID       string
	daemonName     string
	allowedOrigins []string
	codeMgr        *CodeManager
	onPairComplete func(token, origin string) error
	statusHandler  func() interface{}
	reloadHandler  func() interface{}
	proxyHandlers  ProxyEndpoints
	mux            *http.ServeMux
	server         *http.Server
}

// ProxyEndpoints bundles the HTTP-level proxy handlers so the daemon
// can keep proxy-specific JSON decoding inside its own package and
// just hand this struct over to pairing.Server.
type ProxyEndpoints struct {
	Status         func(w http.ResponseWriter, r *http.Request)
	Configure      func(w http.ResponseWriter, r *http.Request)
	Enable         func(w http.ResponseWriter, r *http.Request)
	Disable        func(w http.ResponseWriter, r *http.Request)
	Restart        func(w http.ResponseWriter, r *http.Request)
	SetMode        func(w http.ResponseWriter, r *http.Request)
	TrustCA        func(w http.ResponseWriter, r *http.Request)
	InstallBinary  func(w http.ResponseWriter, r *http.Request)
}

// ServerConfig holds configuration for the pairing server.
type ServerConfig struct {
	Port           int
	DaemonID       string
	DaemonName     string
	AllowedOrigins []string
	OnPairComplete func(token, origin string) error
	StatusHandler  func() interface{}
	ReloadHandler  func() interface{}
	// ProxyHandlers exposes /api/proxy/* endpoints. All fields optional;
	// unset handlers return 404 so older daemon builds stay compatible.
	ProxyHandlers ProxyEndpoints
}

// NewServer creates a new pairing HTTP server.
func NewServer(cfg ServerConfig) *Server {
	s := &Server{
		port:           cfg.Port,
		daemonID:       cfg.DaemonID,
		daemonName:     cfg.DaemonName,
		allowedOrigins: cfg.AllowedOrigins,
		codeMgr:        NewCodeManager(),
		onPairComplete: cfg.OnPairComplete,
		statusHandler:  cfg.StatusHandler,
		reloadHandler:  cfg.ReloadHandler,
		proxyHandlers:  cfg.ProxyHandlers,
		mux:            http.NewServeMux(),
	}

	s.mux.HandleFunc("/api/pairing/code", s.handleCORS(s.handlePairingCode))
	s.mux.HandleFunc("/api/pairing/complete", s.handleCORS(s.handlePairingComplete))
	s.mux.HandleFunc("/api/status", s.handleCORS(s.handleStatus))
	s.mux.HandleFunc("/api/services/reload", s.handleCORS(s.handleReload))

	// Proxy lifecycle endpoints — first-class, not a service-bundle hack.
	// Each handler is optional; unset paths return 404 via handleOpt404.
	s.mux.HandleFunc("/api/proxy/status", s.handleCORS(s.handleOpt404(cfg.ProxyHandlers.Status)))
	s.mux.HandleFunc("/api/proxy/configure", s.handleCORS(s.handleOpt404(cfg.ProxyHandlers.Configure)))
	s.mux.HandleFunc("/api/proxy/enable", s.handleCORS(s.handleOpt404(cfg.ProxyHandlers.Enable)))
	s.mux.HandleFunc("/api/proxy/disable", s.handleCORS(s.handleOpt404(cfg.ProxyHandlers.Disable)))
	s.mux.HandleFunc("/api/proxy/restart", s.handleCORS(s.handleOpt404(cfg.ProxyHandlers.Restart)))
	s.mux.HandleFunc("/api/proxy/set-mode", s.handleCORS(s.handleOpt404(cfg.ProxyHandlers.SetMode)))
	s.mux.HandleFunc("/api/proxy/trust-ca", s.handleCORS(s.handleOpt404(cfg.ProxyHandlers.TrustCA)))
	s.mux.HandleFunc("/api/proxy/install-binary", s.handleCORS(s.handleOpt404(cfg.ProxyHandlers.InstallBinary)))

	return s
}

// handleOpt404 wraps an optional handler. Returns a 404 handler when
// the underlying handler is nil so older daemon builds — or features
// not yet implemented — don't panic.
func (s *Server) handleOpt404(h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	if h != nil {
		return h
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not implemented", http.StatusNotFound)
	}
}

// Start begins listening on localhost.
func (s *Server) Start() error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("binding to %s: %w", addr, err)
	}

	s.server = &http.Server{
		Handler:      s.mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Warn("pairing server error", "err", err)
		}
	}()

	slog.Info("pairing server listening", "addr", addr)
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() {
	if s.server != nil {
		_ = s.server.Close()
	}
}

func (s *Server) handlePairingCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	code, nonce, expiresAt := s.codeMgr.CurrentCode()

	resp := map[string]interface{}{
		"daemon_id":  s.daemonID,
		"code":       code,
		"nonce":      nonce,
		"name":       s.daemonName,
		"expires_at": expiresAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePairingComplete(w http.ResponseWriter, r *http.Request) {
	// OPTIONS is already handled by the CORS wrapper.
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Validate Origin header.
	origin := r.Header.Get("Origin")
	if !s.isAllowedOrigin(origin) {
		http.Error(w, "forbidden: invalid origin", http.StatusForbidden)
		return
	}

	var req struct {
		Code            string `json:"code"`
		Nonce           string `json:"nonce"`
		ConnectionToken string `json:"connection_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Validate code + nonce.
	if !s.codeMgr.Validate(req.Code, req.Nonce) {
		http.Error(w, "forbidden: invalid code or nonce", http.StatusForbidden)
		return
	}

	// Derive cloud origin from the validated HTTP Origin header.
	if s.onPairComplete != nil {
		if err := s.onPairComplete(req.ConnectionToken, origin); err != nil {
			http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.statusHandler != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.statusHandler())
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.reloadHandler != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.reloadHandler())
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if s.isAllowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Vary", "Origin")
		} else if origin != "" {
			http.Error(w, "forbidden: origin not allowed", http.StatusForbidden)
			return
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next(w, r)
	}
}

func (s *Server) isAllowedOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	for _, allowed := range s.allowedOrigins {
		if strings.EqualFold(origin, allowed) {
			return true
		}
	}
	return false
}
