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

// Server is the localhost HTTP server for pairing and status.
type Server struct {
	port            int
	daemonID        string
	daemonName      string
	allowedOrigins  []string
	codeMgr         *CodeManager
	onPairComplete  func(token, origin string) error
	statusHandler   func() interface{}
	reloadHandler   func() interface{}
	mux             *http.ServeMux
	server          *http.Server
}

// ServerConfig holds configuration for the pairing server.
type ServerConfig struct {
	Port            int
	DaemonID        string
	DaemonName      string
	AllowedOrigins  []string
	OnPairComplete  func(token, origin string) error
	StatusHandler   func() interface{}
	ReloadHandler   func() interface{}
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
		mux:            http.NewServeMux(),
	}

	s.mux.HandleFunc("/api/pairing/code", s.handleCORS(s.handlePairingCode))
	s.mux.HandleFunc("/api/pairing/complete", s.handleCORS(s.handlePairingComplete))
	s.mux.HandleFunc("/api/status", s.handleCORS(s.handleStatus))
	s.mux.HandleFunc("/api/services/reload", s.handleCORS(s.handleReload))

	return s
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
