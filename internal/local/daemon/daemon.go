package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/local/config"
	"github.com/clawvisor/clawvisor/internal/local/executor"
	"github.com/clawvisor/clawvisor/internal/local/pairing"
	"github.com/clawvisor/clawvisor/internal/local/proxy"
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
	// proxy is the first-class Clawvisor Network Proxy lifecycle.
	// Nil until Run() constructs it; always non-nil thereafter.
	proxy     *proxy.Manager
	logger    *slog.Logger
	startTime time.Time

	cfgMu sync.RWMutex

	// mu protects state, tunnelClient, and connected.
	mu           sync.RWMutex
	state        *state.State
	tunnelClient *tunnel.Client
}

// Proxy returns the proxy lifecycle manager. Callers can check for nil
// before Run() is called; after Run() returns it is guaranteed non-nil.
func (d *Daemon) Proxy() *proxy.Manager { return d.proxy }

// StatusSummary is a compact view of daemon state for local UI surfaces.
type StatusSummary struct {
	DaemonID      string
	Name          string
	Paired        bool
	Connected     bool
	CloudOrigin   string
	UptimeSeconds int
	ServiceCount  int
	ExcludedCount int
}

// ServiceOption represents a service that can be enabled or disabled by the user.
type ServiceOption struct {
	Key        string
	ID         string
	Name       string
	Enabled    bool
	Toggleable bool
	Problem    string
	Tooltip    string
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
	d.cfgMu.Lock()
	d.cfg.Port = port
	d.cfgMu.Unlock()
}

// Run starts the daemon and blocks until shutdown.
func (d *Daemon) Run() error {
	ctx, stop := signalContext()
	defer stop()
	return d.RunContext(ctx)
}

// RunContext starts the daemon and blocks until the context is cancelled.
func (d *Daemon) RunContext(ctx context.Context) error {
	d.startTime = time.Now()
	cfg := d.configSnapshot()

	// Configure log level.
	configureLogLevel(cfg.LogLevel)

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

	// Bring up the Clawvisor Network Proxy lifecycle. Config is loaded
	// from ~/.clawvisor/proxy/config.json; if missing or enabled=false,
	// the manager stays idle. cmd_proxy.go drives configuration +
	// enable/disable via the pairing server's /api/proxy/* routes.
	d.proxy = proxy.New(d.baseDir, d.logger.With("component", "proxy"))
	if err := d.proxy.LoadConfig(); err != nil {
		d.logger.Warn("proxy: load config failed", "err", err)
	}
	if d.proxy.Status().Enabled {
		if err := d.proxy.Enable(); err != nil {
			d.logger.Warn("proxy: auto-enable on boot failed", "err", err)
		}
	}

	// Discover and load services.
	d.reloadServices()

	// Create dispatcher.
	d.dispatcher = executor.NewDispatcher(
		d.registry,
		d.serverMgr,
		cfg.Env,
		cfg.MaxOutputSize,
		cfg.MaxConcurrentReqs,
	)

	// Start pairing HTTP server.
	d.pairServer = pairing.NewServer(pairing.ServerConfig{
		Port:           cfg.Port,
		DaemonID:       daemonID,
		DaemonName:     cfg.Name,
		AllowedOrigins: cfg.AllowedCloudOrigins,
		OnPairComplete: d.handlePairComplete,
		StatusHandler:  d.handleStatus,
		ReloadHandler:  d.handleReload,
		ProxyHandlers:  d.proxyEndpoints(),
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
	if cfg.ScanInterval > 0 {
		scanTicker = time.NewTicker(time.Duration(cfg.ScanInterval) * time.Second)
		go func() {
			for range scanTicker.C {
				d.reloadServices()
			}
		}()
	}

	<-ctx.Done()

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

	// Stop proxy (preserves Enabled=true so it comes back on next boot).
	if d.proxy != nil {
		d.proxy.Stop()
	}

	// Stop pairing server.
	d.pairServer.Stop()

	return nil
}

// Summary returns a compact daemon status snapshot.
func (d *Daemon) Summary() StatusSummary {
	d.mu.RLock()
	tc := d.tunnelClient
	daemonID := d.state.DaemonID
	paired := d.state.IsPaired()
	cloudOrigin := d.state.CloudOrigin
	d.mu.RUnlock()

	serviceCount := 0
	excludedCount := 0
	if d.registry != nil {
		serviceCount = len(d.registry.All())
		excludedCount = len(d.registry.Excluded())
	}

	uptime := 0
	if !d.startTime.IsZero() {
		uptime = int(time.Since(d.startTime).Seconds())
	}

	return StatusSummary{
		DaemonID:      daemonID,
		Name:          d.configSnapshot().Name,
		Paired:        paired,
		Connected:     tc != nil && tc.IsConnected(),
		CloudOrigin:   cloudOrigin,
		UptimeSeconds: uptime,
		ServiceCount:  serviceCount,
		ExcludedCount: excludedCount,
	}
}

// ReloadServices re-scans manifests and refreshes tunnel capabilities.
func (d *Daemon) ReloadServices() {
	d.reloadServices()
}

// KeepAwakeEnabled reports whether the daemon should keep the machine awake.
func (d *Daemon) KeepAwakeEnabled() bool {
	return d.configSnapshot().KeepAwake
}

// SetKeepAwake updates and persists the keep-awake preference.
func (d *Daemon) SetKeepAwake(enabled bool) error {
	return d.updateConfig(func(cfg *config.Config) {
		cfg.KeepAwake = enabled
	})
}

// AutoUpdateEnabled reports whether automatic updates are enabled.
func (d *Daemon) AutoUpdateEnabled() bool {
	return d.configSnapshot().AutoUpdateEnabled
}

// AutoUpdateInterval reports how often update checks should run.
func (d *Daemon) AutoUpdateInterval() time.Duration {
	return d.configSnapshot().AutoUpdateInterval.Duration
}

// SetAutoUpdateEnabled updates and persists the auto-update preference.
func (d *Daemon) SetAutoUpdateEnabled(enabled bool) error {
	return d.updateConfig(func(cfg *config.Config) {
		cfg.AutoUpdateEnabled = enabled
	})
}

// ServiceOptions returns discoverable services and whether each is enabled.
func (d *Daemon) ServiceOptions() []ServiceOption {
	cfg := d.configSnapshot()
	result := services.Discover(cfg.ServiceDirs, cfg.DefaultTimeout.Duration, disabledServiceSet(cfg.DisabledServices))
	seen := make(map[string]ServiceOption)

	for _, svc := range result.Services {
		seen["service:"+svc.ID] = ServiceOption{
			Key:        "service:" + svc.ID,
			ID:         svc.ID,
			Name:       svc.Name,
			Enabled:    true,
			Toggleable: true,
			Tooltip:    svc.ID,
		}
	}
	for _, ex := range result.Excluded {
		if ex.Category == "disabled" && ex.ID != "" {
			seen["service:"+ex.ID] = ServiceOption{
				Key:        "service:" + ex.ID,
				ID:         ex.ID,
				Name:       ex.Name,
				Enabled:    false,
				Toggleable: true,
				Tooltip:    ex.ID,
			}
			continue
		}

		name := ex.Name
		if name == "" {
			name = strings.TrimSuffix(ex.Path, "/service.yaml")
			name = strings.TrimSuffix(name, "service.yaml")
			name = strings.TrimSuffix(name, "/")
			if name == "" {
				name = "Unknown service"
			}
		}
		key := "problem:" + ex.Path + ":" + ex.Category
		seen[key] = ServiceOption{
			Key:        key,
			ID:         ex.ID,
			Name:       fmt.Sprintf("%s (%s)", name, serviceProblemLabel(ex.Category)),
			Enabled:    false,
			Toggleable: false,
			Problem:    ex.Category,
			Tooltip:    ex.Error,
		}
	}

	options := make([]ServiceOption, 0, len(seen))
	for _, opt := range seen {
		options = append(options, opt)
	}
	slices.SortFunc(options, func(a, b ServiceOption) int {
		return strings.Compare(a.Name+"|"+a.Key, b.Name+"|"+b.Key)
	})
	return options
}

// SetServiceEnabled updates and persists whether a service should be loaded.
func (d *Daemon) SetServiceEnabled(serviceID string, enabled bool) error {
	if serviceID == "" {
		return fmt.Errorf("service ID is required")
	}

	return d.updateConfig(func(cfg *config.Config) {
		current := make(map[string]bool, len(cfg.DisabledServices))
		for _, id := range cfg.DisabledServices {
			if id != "" {
				current[id] = true
			}
		}

		if enabled {
			delete(current, serviceID)
		} else {
			current[serviceID] = true
		}

		updated := make([]string, 0, len(current))
		for id := range current {
			updated = append(updated, id)
		}
		slices.Sort(updated)
		cfg.DisabledServices = updated
	})
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
	name := d.configSnapshot().Name

	d.mu.Lock()

	// Close any existing tunnel client to avoid duplicate connections.
	if d.tunnelClient != nil {
		d.tunnelClient.Close()
	}

	var client *tunnel.Client
	client = tunnel.NewClient(tunnel.ClientConfig{
		CloudOrigin:     d.state.CloudOrigin,
		ConnectionToken: d.state.ConnectionToken,
		DaemonName:      name,
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
	cfg := d.configSnapshot()
	result := services.Discover(cfg.ServiceDirs, cfg.DefaultTimeout.Duration, disabledServiceSet(cfg.DisabledServices))

	if d.serverMgr == nil {
		d.registry.Load(result)
		return
	}

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

func disabledServiceSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		out[id] = true
	}
	return out
}

func serviceProblemLabel(category string) string {
	switch category {
	case "invalid":
		return "Invalid config"
	case "conflict":
		return "Duplicate ID"
	case "unsupported":
		return "Unsupported"
	default:
		return "Unavailable"
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
		"name":              d.configSnapshot().Name,
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

func (d *Daemon) configSnapshot() *config.Config {
	d.cfgMu.RLock()
	defer d.cfgMu.RUnlock()
	return cloneConfig(d.cfg)
}

func (d *Daemon) updateConfig(mutator func(*config.Config)) error {
	d.cfgMu.Lock()
	mutator(d.cfg)
	snapshot := cloneConfig(d.cfg)
	d.cfgMu.Unlock()
	return config.Save(d.baseDir, snapshot)
}

func cloneConfig(cfg *config.Config) *config.Config {
	if cfg == nil {
		return nil
	}
	clone := *cfg
	if cfg.ServiceDirs != nil {
		clone.ServiceDirs = append([]string(nil), cfg.ServiceDirs...)
	}
	if cfg.DisabledServices != nil {
		clone.DisabledServices = append([]string(nil), cfg.DisabledServices...)
	}
	if cfg.AllowedCloudOrigins != nil {
		clone.AllowedCloudOrigins = append([]string(nil), cfg.AllowedCloudOrigins...)
	}
	if cfg.Env != nil {
		clone.Env = make(map[string]string, len(cfg.Env))
		for key, value := range cfg.Env {
			clone.Env[key] = value
		}
	}
	return &clone
}
