package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Manager supervises a single Clawvisor Network Proxy child process.
// It persists config under ~/.clawvisor/proxy/, starts the binary with
// the admin endpoint on a Unix socket (for local health checks), and
// monitors/restarts with exponential backoff on crash.
//
// Thread-safety: Enable/Disable/Restart/Configure are serialized
// through mu. Status() can be called concurrently and returns a
// snapshot with no external locking.
type Manager struct {
	logger *slog.Logger

	// dataDir holds CA cert, signing keys, traffic log (the proxy's --data-dir).
	dataDir string
	// stateDir holds config.json + proxy_token file + admin socket.
	stateDir string

	mu           sync.Mutex
	cfg          Config
	token        string
	state        State
	cmd          *exec.Cmd
	done         chan struct{}
	startedAt    time.Time
	restartCount int
	lastError    string
	// supervisor goroutine cancel — set on Enable, cleared on Disable.
	cancel context.CancelFunc
}

// New constructs a Manager but does not start the proxy. Callers should
// call LoadConfig() on boot to rehydrate from disk; if cfg.Enabled is
// true, Enable() starts the process.
func New(baseDir string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		logger:   logger,
		dataDir:  filepath.Join(baseDir, "proxy-data"),
		stateDir: filepath.Join(baseDir, "proxy"),
		state:    StateDisabled,
		cfg:      Defaults(),
	}
}

// DataDir exposes the proxy's --data-dir. The CA cert lives at
// <data_dir>/ca.pem after first run.
func (m *Manager) DataDir() string { return m.dataDir }

// CACertPath is the conventional path consumers pass via
// NODE_EXTRA_CA_CERTS / SSL_CERT_FILE.
func (m *Manager) CACertPath() string { return filepath.Join(m.dataDir, "ca.pem") }

// adminSocket is the Unix-socket path the proxy's admin endpoint
// listens on. A hash keeps the path short + stable for launchd paths
// even if stateDir is long.
func (m *Manager) adminSocket() string {
	h := sha256.Sum256([]byte(m.stateDir))
	return filepath.Join(m.stateDir, "admin-"+hex.EncodeToString(h[:4])+".sock")
}

// tokenPath is the 0600 file that stores the cvisproxy_ token.
// Split from config.json so dumping the config for diagnostics is safe.
func (m *Manager) tokenPath() string { return filepath.Join(m.stateDir, "proxy-token") }

// configPath is the persisted config JSON.
func (m *Manager) configPath() string { return filepath.Join(m.stateDir, "config.json") }

// migrateLegacyDataDir moves data from the pre-refactor location
// (~/.clawvisor/proxy-data/) into the daemon-owned dir (m.dataDir,
// typically ~/.clawvisor/local/proxy-data/). Idempotent — if the new
// dir already has a CA, the legacy dir is left alone (last-write-wins
// would be wrong since the keychain may already trust the new CA).
//
// This survives the install path drift from an earlier UX iteration
// where the proxy ran out of ~/.clawvisor/proxy-data/ before being
// adopted by the daemon.
func (m *Manager) migrateLegacyDataDir() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	legacy := filepath.Join(home, ".clawvisor", "proxy-data")
	legacyCA := filepath.Join(legacy, "ca.pem")
	if _, err := os.Stat(legacyCA); err != nil {
		return nil // nothing to migrate
	}
	newCA := filepath.Join(m.dataDir, "ca.pem")
	if _, err := os.Stat(newCA); err == nil {
		return nil // new path already has a CA — don't overwrite
	}
	if err := os.MkdirAll(m.dataDir, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(legacy)
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := filepath.Join(legacy, e.Name())
		dst := filepath.Join(m.dataDir, e.Name())
		if err := os.Rename(src, dst); err != nil {
			// Best-effort — keep going. A partial migration is
			// recoverable; a hard failure here would block the daemon.
			m.logger.Warn("proxy: migrate file", "src", src, "err", err)
		}
	}
	_ = os.Remove(legacy) // leaves it if non-empty
	return nil
}

// -- Config persistence --------------------------------------------------

// LoadConfig rehydrates cfg + token from disk. Safe to call on a
// fresh machine — returns nil and leaves Defaults() in place if no
// config exists yet. Also migrates the legacy data dir if found.
func (m *Manager) LoadConfig() error {
	if err := m.migrateLegacyDataDir(); err != nil {
		// Non-fatal — log via the logger field once we have it.
		// Falling through means a fresh CA gets generated, which is
		// recoverable but forces re-trust.
		m.logger.Warn("proxy: legacy data dir migration failed", "err", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := os.ReadFile(m.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read proxy config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse proxy config: %w", err)
	}
	m.cfg = cfg
	tokRaw, err := os.ReadFile(m.tokenPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read proxy token: %w", err)
	}
	m.token = string(tokRaw)
	return nil
}

// Configure replaces the persisted config (and token if token != "").
// Does not change the running state — caller decides when to Enable.
// Returns an error if the config fails validation (missing required
// fields) so invalid state is never persisted.
func (m *Manager) Configure(cfg Config, token string) error {
	if cfg.BridgeID == "" {
		return errors.New("bridge_id is required")
	}
	if token == "" && m.token == "" {
		return errors.New("proxy token is required on first configure")
	}
	if cfg.ListenPort <= 0 {
		cfg.ListenPort = 25298
	}
	if cfg.ListenHost == "" {
		cfg.ListenHost = "127.0.0.1"
	}
	if cfg.ServerURL == "" {
		cfg.ServerURL = "http://127.0.0.1:25297"
	}
	if cfg.Mode == "" {
		cfg.Mode = "observe"
	}
	if cfg.Mode != "observe" && cfg.Mode != "enforce" {
		return fmt.Errorf("mode must be observe or enforce, got %q", cfg.Mode)
	}
	if cfg.BinaryPath == "" {
		return errors.New("binary_path is required")
	}
	if _, err := os.Stat(cfg.BinaryPath); err != nil {
		return fmt.Errorf("binary_path %s: %w", cfg.BinaryPath, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(m.stateDir, 0700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	if err := os.MkdirAll(m.dataDir, 0700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.configPath(), raw, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if token != "" {
		if err := os.WriteFile(m.tokenPath(), []byte(token), 0600); err != nil {
			return fmt.Errorf("write token: %w", err)
		}
		m.token = token
	}
	m.cfg = cfg
	return nil
}

// -- Lifecycle ----------------------------------------------------------

// Enable marks the proxy as enabled, persists it, and starts the
// process + supervisor. Idempotent: calling Enable on a running proxy
// returns nil without restarting.
func (m *Manager) Enable() error {
	m.mu.Lock()
	if m.cfg.BridgeID == "" || m.token == "" || m.cfg.BinaryPath == "" {
		m.mu.Unlock()
		return errors.New("proxy not configured — call Configure first")
	}
	if m.state == StateRunning || m.state == StateStarting {
		m.mu.Unlock()
		return nil
	}
	m.cfg.Enabled = true
	if err := m.persistEnabledLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	return m.startLocked(false)
}

// Disable stops the process (if running) and persists Enabled=false so
// the daemon doesn't restart it on next boot.
func (m *Manager) Disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Enabled = false
	if err := m.persistEnabledLocked(); err != nil {
		return err
	}
	m.stopProcessLocked()
	m.state = StateDisabled
	return nil
}

// Restart kills the current process and starts a fresh one. Useful when
// the binary changes (version upgrade) or config is rewritten.
func (m *Manager) Restart() error {
	m.mu.Lock()
	if !m.cfg.Enabled {
		m.mu.Unlock()
		return errors.New("proxy not enabled")
	}
	m.stopProcessLocked()
	m.mu.Unlock()
	return m.startLocked(true)
}

// Stop is called on daemon shutdown. Stops the process but preserves
// Enabled=true so the proxy comes back on next daemon start.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopProcessLocked()
}

// Status returns an atomic snapshot. Lock briefly to copy fields.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{
		State:        m.state,
		Enabled:      m.cfg.Enabled,
		ListenHost:   m.cfg.ListenHost,
		ListenPort:   m.cfg.ListenPort,
		BridgeID:     m.cfg.BridgeID,
		ServerURL:    m.cfg.ServerURL,
		Mode:         m.cfg.Mode,
		StartedAt:    m.startedAt,
		RestartCount: m.restartCount,
		LastError:    m.lastError,
		BinaryPath:   m.cfg.BinaryPath,
		CACertPath:   m.CACertPath(),
	}
	if m.cmd != nil && m.cmd.Process != nil {
		s.PID = m.cmd.Process.Pid
	}
	return s
}

// -- internals ----------------------------------------------------------

// persistEnabledLocked rewrites config.json to reflect the current
// m.cfg.Enabled bit. Called with mu held.
func (m *Manager) persistEnabledLocked() error {
	if err := os.MkdirAll(m.stateDir, 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.configPath(), raw, 0600)
}

// startLocked launches the proxy process and blocks until the admin
// health check returns 200 or timeout. If isRestart is true,
// restartCount is bumped so diagnostics distinguish "fresh start" from
// "crash recovery."
func (m *Manager) startLocked(isRestart bool) error {
	m.mu.Lock()
	m.state = StateStarting
	m.lastError = ""
	if isRestart {
		m.restartCount++
	}
	cfg := m.cfg
	tok := m.token
	adminSock := m.adminSocket()
	m.mu.Unlock()

	// Fresh socket — old supervisor may have left one behind.
	_ = os.Remove(adminSock)
	if err := os.MkdirAll(filepath.Dir(adminSock), 0700); err != nil {
		return m.failStart(fmt.Errorf("create state dir: %w", err))
	}

	// Write the small YAML config the proxy reads.
	cfgYAML := fmt.Sprintf(
		"server_url: %q\nproxy_token: %q\nbridge_id: %q\n",
		cfg.ServerURL, tok, cfg.BridgeID,
	)
	cfgPath := filepath.Join(m.stateDir, "clawvisor.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0600); err != nil {
		return m.failStart(fmt.Errorf("write clawvisor.yaml: %w", err))
	}

	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.Command(cfg.BinaryPath, //nolint:gosec
		"serve",
		"--mode="+cfg.Mode,
		"--host="+cfg.ListenHost,
		"--port="+fmt.Sprintf("%d", cfg.ListenPort),
		"--data-dir="+m.dataDir,
		"--admin-socket="+adminSock,
		"--clawvisor-config="+cfgPath,
	)
	cmd.Env = append(os.Environ(), "CLAWVISOR_SOCKET="+adminSock)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Capture stderr into a ring buffer via the logger.
	stderrPipe, _ := cmd.StderrPipe()
	stdoutPipe, _ := cmd.StdoutPipe()

	if err := cmd.Start(); err != nil {
		cancel()
		return m.failStart(fmt.Errorf("start proxy: %w", err))
	}

	// Forward stdout/stderr to our logger so `launchctl log show` + the
	// user's toolbar show proxy diagnostics without opening a sub-log.
	if stdoutPipe != nil {
		go pipeLog(m.logger.With("stream", "stdout"), stdoutPipe)
	}
	if stderrPipe != nil {
		go pipeLog(m.logger.With("stream", "stderr"), stderrPipe)
	}

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	m.mu.Lock()
	m.cmd = cmd
	m.done = done
	m.cancel = cancel
	m.mu.Unlock()

	// Block until admin health check succeeds or timeout.
	if err := m.waitForHealthy(ctx, adminSock, done, 15*time.Second); err != nil {
		m.mu.Lock()
		m.stopProcessLocked()
		m.mu.Unlock()
		return m.failStart(err)
	}

	m.mu.Lock()
	m.state = StateRunning
	m.startedAt = time.Now()
	m.mu.Unlock()
	m.logger.Info("proxy started",
		"pid", cmd.Process.Pid, "port", cfg.ListenPort, "mode", cfg.Mode)

	// Supervisor: if the process exits unexpectedly, try to restart
	// with exponential backoff. Cancelled by Disable/Stop.
	go m.superviseLocked(ctx, done)
	return nil
}

func (m *Manager) failStart(err error) error {
	m.mu.Lock()
	m.state = StateFailed
	m.lastError = err.Error()
	m.mu.Unlock()
	m.logger.Warn("proxy start failed", "err", err)
	return err
}

// stopProcessLocked kills the current process (if any) and waits for
// exit. Called with mu held; does not touch m.state or m.cfg.Enabled.
func (m *Manager) stopProcessLocked() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if m.cmd == nil || m.cmd.Process == nil {
		return
	}
	pid := m.cmd.Process.Pid
	_ = m.cmd.Process.Signal(syscall.SIGTERM)
	done := m.done
	m.mu.Unlock() // release so the Wait goroutine can see the exit
	timer := time.NewTimer(5 * time.Second)
	select {
	case <-done:
		timer.Stop()
	case <-timer.C:
		_ = m.cmd.Process.Kill()
		<-done
	}
	m.mu.Lock()
	m.cmd = nil
	m.done = nil
	m.state = StateStopped
	m.logger.Info("proxy stopped", "pid", pid)
}

// superviseLocked blocks until either ctx is cancelled (Disable/Stop)
// or the process exits. On unexpected exit, restart with backoff.
func (m *Manager) superviseLocked(ctx context.Context, done <-chan struct{}) {
	select {
	case <-ctx.Done():
		return
	case <-done:
	}

	m.mu.Lock()
	wasExpected := m.cancel == nil // stopProcessLocked cleared it
	m.mu.Unlock()
	if wasExpected {
		return
	}

	m.mu.Lock()
	m.state = StateFailed
	m.lastError = "process exited unexpectedly"
	delay := restartBackoff(m.restartCount)
	m.mu.Unlock()

	m.logger.Warn("proxy exited unexpectedly; restarting", "delay", delay)
	time.Sleep(delay)
	_ = m.Restart()
}

// restartBackoff mirrors the shape in the daemon's ServerManager so
// operational intuition carries over.
func restartBackoff(n int) time.Duration {
	delays := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second,
	}
	if n >= len(delays) {
		n = len(delays) - 1
	}
	return delays[n]
}

// waitForHealthy polls the admin socket's /health until a 200 or
// deadline. Exits early if the process dies.
func (m *Manager) waitForHealthy(ctx context.Context, sock string, done <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sock, 2*time.Second)
			},
		},
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return errors.New("proxy process exited during startup")
		default:
		}
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://unix/health", nil)
		resp, err := client.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("proxy health check timed out after %s", timeout)
}

// pipeLog scans lines from r and forwards each to logger. The clawvisor-proxy
// binary writes both log-format lines and plain status banners; we
// don't try to parse, just line-by-line copy.
func pipeLog(logger *slog.Logger, r io.ReadCloser) {
	defer r.Close()
	buf := make([]byte, 4096)
	var partial []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append(partial, buf[:n]...)
			for {
				idx := indexByte(chunk, '\n')
				if idx < 0 {
					partial = chunk
					break
				}
				line := chunk[:idx]
				if len(line) > 0 {
					logger.Info("clawvisor-proxy", "line", string(line))
				}
				chunk = chunk[idx+1:]
			}
		}
		if err != nil {
			if len(partial) > 0 {
				logger.Info("clawvisor-proxy", "line", string(partial))
			}
			return
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
