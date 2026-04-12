package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/clawvisor/clawvisor/internal/local/config"
	"github.com/clawvisor/clawvisor/internal/local/executor"
	"github.com/clawvisor/clawvisor/internal/local/pairing"
	"github.com/clawvisor/clawvisor/internal/local/services"
	"github.com/clawvisor/clawvisor/internal/local/state"
	"github.com/clawvisor/clawvisor/internal/local/tunnel"
	"github.com/clawvisor/clawvisor/pkg/version"
)

// Daemon is the top-level orchestrator for the clawvisor local daemon.
type Daemon struct {
	baseDir    string
	cfg        *config.Config
	registry   *services.Registry
	serverMgr  *executor.ServerManager
	dispatcher *executor.Dispatcher
	pairServer *pairing.Server
	logger     *slog.Logger
	startTime  time.Time

	// mu protects state, tunnelClient, and connected.
	mu           sync.RWMutex
	state        *state.State
	tunnelClient *tunnel.Client
}

// New creates a new daemon instance.
func New(baseDir string) (*Daemon, error) {
	cfg, err := config.Load(baseDir)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	st, err := state.Load(baseDir)
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}

	// Persist the daemon ID if it was just generated.
	if err := state.Save(baseDir, st); err != nil {
		return nil, fmt.Errorf("saving initial state: %w", err)
	}

	return &Daemon{
		baseDir:  baseDir,
		cfg:      cfg,
		state:    st,
		registry: services.NewRegistry(),
		logger:   slog.Default(),
	}, nil
}

// OverridePort overrides the configured port for the pairing server.
func (d *Daemon) OverridePort(port int) {
	d.cfg.Port = port
}

// Run starts the daemon and blocks until shutdown.
func (d *Daemon) Run() error {
	d.startTime = time.Now()

	// Configure log level.
	configureLogLevel(d.cfg.LogLevel)

	d.mu.RLock()
	daemonID := d.state.DaemonID
	paired := d.state.IsPaired()
	d.mu.RUnlock()

	d.logger.Info("starting", "version", version.Version, "daemon_id", daemonID)

	// Initialize server manager.
	d.serverMgr = executor.NewServerManager(d.baseDir)
	if err := d.serverMgr.Init(); err != nil {
		return fmt.Errorf("initializing server manager: %w", err)
	}

	// Discover and load services.
	d.reloadServices()

	// Create dispatcher.
	d.dispatcher = executor.NewDispatcher(
		d.registry,
		d.serverMgr,
		d.cfg.Env,
		d.cfg.MaxOutputSize,
		d.cfg.MaxConcurrentReqs,
	)

	// Start pairing HTTP server.
	d.pairServer = pairing.NewServer(pairing.ServerConfig{
		Port:           d.cfg.Port,
		DaemonID:       daemonID,
		DaemonName:     d.cfg.Name,
		AllowedOrigins: d.cfg.AllowedCloudOrigins,
		OnPairComplete: d.handlePairComplete,
		StatusHandler:  d.handleStatus,
		ReloadHandler:  d.handleReload,
	})

	if err := d.pairServer.Start(); err != nil {
		return fmt.Errorf("starting pairing server: %w", err)
	}

	// If already paired, connect to cloud.
	if paired {
		d.connectTunnel()
	}

	// Set up periodic scan if configured.
	var scanTicker *time.Ticker
	if d.cfg.ScanInterval > 0 {
		scanTicker = time.NewTicker(time.Duration(d.cfg.ScanInterval) * time.Second)
		go func() {
			for range scanTicker.C {
				d.reloadServices()
			}
		}()
	}

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	d.logger.Info("shutting down")

	if scanTicker != nil {
		scanTicker.Stop()
	}

	// Stop tunnel.
	d.mu.RLock()
	tc := d.tunnelClient
	d.mu.RUnlock()
	if tc != nil {
		tc.Close()
	}

	// Stop server processes.
	d.serverMgr.StopAll()

	// Stop pairing server.
	d.pairServer.Stop()

	return nil
}

func (d *Daemon) handlePairComplete(token, origin string) error {
	d.mu.Lock()
	d.state.ConnectionToken = token
	d.state.CloudOrigin = origin
	d.state.PairedAt = time.Now()
	st := d.state
	d.mu.Unlock()

	if err := state.Save(d.baseDir, st); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}

	d.logger.Info("paired with cloud", "origin", origin)

	// Connect to cloud.
	d.connectTunnel()

	return nil
}

func (d *Daemon) connectTunnel() {
	d.mu.Lock()

	// Close any existing tunnel client to avoid duplicate connections.
	if d.tunnelClient != nil {
		d.tunnelClient.Close()
	}

	var client *tunnel.Client
	client = tunnel.NewClient(tunnel.ClientConfig{
		CloudOrigin:     d.state.CloudOrigin,
		ConnectionToken: d.state.ConnectionToken,
		DaemonName:      d.cfg.Name,
		Version:         version.Version,
		Registry:        d.registry,
		Logger:          d.logger.With("component", "tunnel"),
		OnRequest:       d.handleRequest,
		OnAuthFailure: func() {
			// Only clear state if this client is still the active one.
			// A stale client from a previous pairing must not wipe the new token.
			d.mu.Lock()
			if d.tunnelClient == client {
				d.handleAuthFailureLocked()
			}
			d.mu.Unlock()
		},
	})
	d.tunnelClient = client

	d.mu.Unlock()

	go client.Connect()
}

func (d *Daemon) handleRequest(ctx context.Context, id string, req *tunnel.RequestPayload) {
	resp := d.dispatcher.Dispatch(ctx, req.Service, req.Action, req.Params, id)

	data, _ := json.Marshal(resp.Data)
	payload := &tunnel.ResponsePayload{
		Success: resp.Success,
		Data:    data,
		Error:   resp.Error,
	}

	d.mu.RLock()
	tc := d.tunnelClient
	d.mu.RUnlock()

	if tc != nil {
		if err := tc.SendResponse(id, payload); err != nil {
			d.logger.Warn("failed to send response", "request_id", id, "err", err)
		}
	}
}

// handleAuthFailureLocked clears pairing state. Caller must hold d.mu.
func (d *Daemon) handleAuthFailureLocked() {
	d.logger.Warn("auth failure, clearing pairing state")
	_ = state.Clear(d.baseDir, d.state.DaemonID)
	d.state.ConnectionToken = ""
	d.state.CloudOrigin = ""
}

func (d *Daemon) reloadServices() {
	result := services.Discover(d.cfg.ServiceDirs, d.cfg.DefaultTimeout.Duration)

	// Track old server hashes for restart decisions.
	oldServices := d.registry.All()
	oldHashes := make(map[string]string)
	for _, svc := range oldServices {
		if svc.Type == "server" {
			oldHashes[svc.ID] = services.RestartHash(svc)
		}
	}

	d.registry.Load(result)

	// Manage server processes.
	newServices := d.registry.All()
	newIDs := make(map[string]bool)

	for _, svc := range newServices {
		newIDs[svc.ID] = true
		if svc.Type == "server" {
			newHash := services.RestartHash(svc)
			if oldHash, existed := oldHashes[svc.ID]; existed {
				if oldHash != newHash {
					// Config changed — restart.
					d.serverMgr.Remove(svc.ID)
					d.serverMgr.Register(svc)
				}
				// Same hash — keep running.
			} else {
				// New service.
				d.serverMgr.Register(svc)
			}
		}
	}

	// Remove servers for deleted services.
	for id := range oldHashes {
		if !newIDs[id] {
			d.serverMgr.Remove(id)
		}
	}

	d.logger.Info("services loaded", "count", len(result.Services), "excluded", len(result.Excluded))

	// Send capabilities update if connected.
	d.mu.RLock()
	tc := d.tunnelClient
	d.mu.RUnlock()
	if tc != nil && tc.IsConnected() {
		if err := tc.SendCapabilitiesUpdate(); err != nil {
			d.logger.Warn("failed to send capabilities update", "err", err)
		}
	}
}

// serviceEntry is the JSON shape for a service in status and reload responses.
type serviceEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Actions []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"actions"`
}

func (d *Daemon) buildServiceList() []serviceEntry {
	svcs := d.registry.All()
	list := make([]serviceEntry, 0, len(svcs))
	for _, svc := range svcs {
		s := serviceEntry{
			ID:   svc.ID,
			Name: svc.Name,
			Type: svc.Type,
		}
		if svc.Type == "server" {
			sp := d.serverMgr.Get(svc.ID)
			if sp != nil {
				s.Status = string(sp.State())
			} else {
				s.Status = "ok"
			}
		} else {
			s.Status = "ok"
		}
		for _, a := range svc.Actions {
			s.Actions = append(s.Actions, struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{ID: a.ID, Name: a.Name})
		}
		list = append(list, s)
	}
	return list
}

func (d *Daemon) handleStatus() interface{} {
	d.mu.RLock()
	tc := d.tunnelClient
	daemonID := d.state.DaemonID
	paired := d.state.IsPaired()
	cloudOrigin := d.state.CloudOrigin
	d.mu.RUnlock()

	connected := tc != nil && tc.IsConnected()
	uptime := time.Since(d.startTime).Seconds()

	return map[string]interface{}{
		"daemon_id":         daemonID,
		"name":              d.cfg.Name,
		"version":           version.Version,
		"paired":            paired,
		"connected":         connected,
		"cloud_origin":      cloudOrigin,
		"uptime_seconds":    int(uptime),
		"services":          d.buildServiceList(),
		"excluded_services": d.registry.Excluded(),
	}
}

func (d *Daemon) handleReload() interface{} {
	d.reloadServices()
	return d.buildServiceList()
}

// configureLogLevel sets the slog default logger level.
func configureLogLevel(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
	})))
}
