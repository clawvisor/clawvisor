package executor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/clawvisor/clawvisor/internal/local/services"
)

// ServerState represents the health state of a server-mode service.
type ServerState string

const (
	ServerStopped       ServerState = "stopped"
	ServerStarting      ServerState = "starting"
	ServerHealthy       ServerState = "healthy"
	ServerStartFailed   ServerState = "starting_failed"
	ServerUnhealthy     ServerState = "unhealthy"
)

// ServerProcess manages a long-running server-mode service process.
type ServerProcess struct {
	mu            sync.Mutex
	svc           *services.Service
	state         ServerState
	cmd           *exec.Cmd
	done          chan struct{} // closed when cmd.Wait() returns
	ready         chan struct{} // closed when startLocked finishes (success or failure)
	socketPath    string
	httpClient    *http.Client
	runDir        string
	startFailures int
	maxFailures   int
}

// ServerManager manages all server-mode service processes.
type ServerManager struct {
	mu        sync.RWMutex
	servers   map[string]*ServerProcess
	runDir    string
}

// NewServerManager creates a new server process manager.
func NewServerManager(baseDir string) *ServerManager {
	runDir := filepath.Join(baseDir, "run")
	return &ServerManager{
		servers: make(map[string]*ServerProcess),
		runDir:  runDir,
	}
}

// Init creates the run directory and cleans up stale sockets.
func (m *ServerManager) Init() error {
	if err := os.MkdirAll(m.runDir, 0700); err != nil {
		return fmt.Errorf("creating run directory: %w", err)
	}
	// Clean stale sockets.
	entries, _ := os.ReadDir(m.runDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sock") {
			_ = os.Remove(filepath.Join(m.runDir, e.Name()))
		}
	}
	return nil
}

// Register registers a server-mode service. If startup is eager, starts it immediately.
func (m *ServerManager) Register(svc *services.Service) {
	socketPath := m.socketPathFor(svc.ID)

	sp := &ServerProcess{
		svc:         svc,
		state:       ServerStopped,
		socketPath:  socketPath,
		runDir:      m.runDir,
		maxFailures: 3,
	}

	m.mu.Lock()
	m.servers[svc.ID] = sp
	m.mu.Unlock()

	if svc.Startup == "eager" {
		go sp.start()
	}
}

// Remove stops and removes a server-mode service.
func (m *ServerManager) Remove(serviceID string) {
	m.mu.Lock()
	sp, ok := m.servers[serviceID]
	if ok {
		delete(m.servers, serviceID)
	}
	m.mu.Unlock()

	if ok {
		sp.stop()
	}
}

// Get returns the server process for a service, or nil.
func (m *ServerManager) Get(serviceID string) *ServerProcess {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.servers[serviceID]
}

// StopAll stops all server processes gracefully.
func (m *ServerManager) StopAll() {
	m.mu.Lock()
	servers := make([]*ServerProcess, 0, len(m.servers))
	for _, sp := range m.servers {
		servers = append(servers, sp)
	}
	m.servers = make(map[string]*ServerProcess)
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, sp := range servers {
		wg.Add(1)
		go func(s *ServerProcess) {
			defer wg.Done()
			s.stop()
		}(sp)
	}
	wg.Wait()
}

func (m *ServerManager) socketPathFor(serviceID string) string {
	h := sha256.Sum256([]byte(serviceID))
	name := fmt.Sprintf("%x.sock", h[:6]) // first 12 hex chars
	return filepath.Join(m.runDir, name)
}

// EnsureRunning makes sure the server is started and healthy before dispatching a request.
func (sp *ServerProcess) EnsureRunning() error {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	for {
		switch sp.state {
		case ServerHealthy:
			return nil
		case ServerStartFailed:
			return fmt.Errorf("server failed to start after %d attempts", sp.maxFailures)
		case ServerStopped, ServerUnhealthy:
			if sp.state == ServerUnhealthy {
				// Apply backoff delay before restart.
				delay := sp.restartBackoff()
				sp.mu.Unlock()
				time.Sleep(delay)
				sp.mu.Lock()
				// Recheck — another goroutine may have started it while we slept.
				continue
			}
			return sp.startLocked()
		case ServerStarting:
			// Already starting — wait for startup to complete without holding the lock.
			ready := sp.ready
			sp.mu.Unlock()
			if ready != nil {
				<-ready
			}
			sp.mu.Lock()
			// Recheck state — startLocked may have failed or succeeded.
			continue
		}
		return nil
	}
}

// State returns the current server state.
func (sp *ServerProcess) State() ServerState {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.state
}

func (sp *ServerProcess) restartBackoff() time.Duration {
	// Backoff: 1s, 2s, 4s, 8s, 16s, 30s (capped).
	delays := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second,
	}
	idx := sp.startFailures
	if idx >= len(delays) {
		idx = len(delays) - 1
	}
	return delays[idx]
}

func (sp *ServerProcess) start() error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.startLocked()
}

func (sp *ServerProcess) startLocked() error {
	sp.state = ServerStarting

	// Create a ready channel so concurrent EnsureRunning callers can wait
	// for startup to complete (not for the process to exit).
	ready := make(chan struct{})
	sp.ready = ready
	defer close(ready)

	// Clean up old socket.
	_ = os.Remove(sp.socketPath)

	argv := sp.svc.Start
	// Resolve relative paths.
	exe := argv[0]
	if strings.HasPrefix(exe, "./") || strings.HasPrefix(exe, "../") {
		exe = filepath.Join(sp.svc.Dir, exe)
	}

	cmd := exec.Command(exe, argv[1:]...)
	cmd.Dir = sp.svc.WorkingDir
	cmd.Env = append(os.Environ(), "CLAWVISOR_SOCKET="+sp.socketPath)
	for k, v := range sp.svc.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		sp.state = ServerStartFailed
		return fmt.Errorf("starting server: %w", err)
	}

	sp.cmd = cmd

	// Create a done channel that closes when the process exits.
	// This is the single Wait point — no other code should call cmd.Wait().
	done := make(chan struct{})
	sp.done = done
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	// Set up HTTP client for Unix socket.
	sp.httpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sp.socketPath, 5*time.Second)
			},
		},
	}

	// Wait for health check — release the lock so health check HTTP requests
	// and concurrent Dispatch calls are not blocked. Pass the done channel
	// so we bail early if the process exits during startup.
	sp.mu.Unlock()
	err := sp.waitForHealthy(done)
	sp.mu.Lock()

	// Recheck: if the process exited during health check, reflect that.
	select {
	case <-done:
		if sp.state == ServerStarting {
			sp.startFailures++
			if sp.startFailures >= sp.maxFailures {
				sp.state = ServerStartFailed
			} else {
				sp.state = ServerUnhealthy
			}
		}
		sp.cmd = nil
		sp.done = nil
		if err == nil {
			err = fmt.Errorf("server process exited during startup")
		}
		return err
	default:
	}

	if err != nil {
		sp.startFailures++
		if sp.startFailures >= sp.maxFailures {
			sp.state = ServerStartFailed
		} else {
			sp.state = ServerUnhealthy
		}
		sp.killLocked()
		return err
	}

	sp.state = ServerHealthy
	sp.startFailures = 0

	// Monitor for unexpected exit.
	go sp.monitor(done)

	return nil
}

func (sp *ServerProcess) waitForHealthy(done <-chan struct{}) error {
	deadline := time.Now().Add(sp.svc.StartupTimeout)
	healthURL := "http://unix" + sp.svc.HealthCheck

	for time.Now().Before(deadline) {
		// Bail early if the process exited.
		select {
		case <-done:
			return fmt.Errorf("server process exited during startup")
		default:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		resp, err := sp.httpClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("server health check timed out after %s", sp.svc.StartupTimeout)
}

// monitor watches for process exit via the done channel (no Wait call needed).
func (sp *ServerProcess) monitor(done <-chan struct{}) {
	<-done

	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.state == ServerHealthy {
		sp.state = ServerUnhealthy
	}
}

func (sp *ServerProcess) stop() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.killLocked()
	_ = os.Remove(sp.socketPath)
}

// killLocked sends SIGTERM, waits for exit via the done channel, then
// SIGKILL if the grace period expires. Does not call Wait directly.
func (sp *ServerProcess) killLocked() {
	if sp.cmd == nil || sp.cmd.Process == nil {
		return
	}

	done := sp.done
	_ = sp.cmd.Process.Signal(syscall.SIGTERM)

	// Release lock while waiting so monitor/other goroutines aren't blocked.
	sp.mu.Unlock()

	timer := time.NewTimer(5 * time.Second)
	select {
	case <-done:
		timer.Stop()
	case <-timer.C:
		_ = sp.cmd.Process.Kill()
		<-done
	}

	sp.mu.Lock()
	sp.cmd = nil
	sp.done = nil
}

// Dispatch sends an HTTP request to the server process for a server-mode action.
func (sp *ServerProcess) Dispatch(
	ctx context.Context,
	action *services.Action,
	params map[string]string,
	maxOutputSize int64,
) *ServerResult {
	if err := sp.EnsureRunning(); err != nil {
		return &ServerResult{Success: false, Error: err.Error()}
	}

	// Apply action timeout to the request context.
	if action.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, action.Timeout)
		defer cancel()
	}

	// Build the HTTP request.
	resolved := resolveParams(action, params)
	reqURL := "http://unix" + action.Path

	var bodyReader io.Reader
	if action.Body != "" {
		interpolated := InterpolateTemplate(action.Body, resolved, action.Params, action.BodyFormat)
		bodyReader = strings.NewReader(interpolated)
	}

	// Add unreferenced params as query parameters.
	// Params not mentioned in the body template are sent as query params.
	{
		unreferenced := make(map[string]string)
		for name, value := range resolved {
			if value == "" {
				continue
			}
			if action.Body != "" && strings.Contains(action.Body, "{{"+name+"}}") {
				continue
			}
			unreferenced[name] = value
		}
		if len(unreferenced) > 0 {
			sep := "?"
			if strings.Contains(reqURL, "?") {
				sep = "&"
			}
			for name, value := range unreferenced {
				reqURL += sep + url.QueryEscape(name) + "=" + url.QueryEscape(value)
				sep = "&"
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, action.Method, reqURL, bodyReader)
	if err != nil {
		return &ServerResult{Success: false, Error: fmt.Sprintf("building request: %s", err)}
	}

	// Merge service-level headers first, then action-level (action takes precedence).
	for k, v := range sp.svc.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range action.Headers {
		req.Header.Set(k, v)
	}
	if action.Body != "" && req.Header.Get("Content-Type") == "" {
		if action.BodyFormat == "json" {
			req.Header.Set("Content-Type", "application/json")
		}
	}

	resp, err := sp.httpClient.Do(req)
	if err != nil {
		return &ServerResult{Success: false, Error: fmt.Sprintf("request failed: %s", err)}
	}
	defer resp.Body.Close()

	// Read body with size limit.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxOutputSize+1))
	bodyResult := ProcessOutput(body, maxOutputSize)

	contentType := resp.Header.Get("Content-Type")

	// Forward response headers.
	respHeaders := make(map[string]string)
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}

	result := &ServerResult{
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Data: &ServerData{
			Status:       resp.StatusCode,
			Body:         bodyResult.Data,
			BodyEncoding: bodyResult.Encoding,
			ContentType:  contentType,
			Headers:      respHeaders,
		},
	}

	if !result.Success {
		result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}

	return result
}

// ServerResult holds the result of a server-mode action invocation.
type ServerResult struct {
	Success bool        `json:"success"`
	Data    *ServerData `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// ServerData holds the output data from a server-mode action.
type ServerData struct {
	Status       int    `json:"status"`
	Body         string `json:"body"`
	BodyEncoding string `json:"body_encoding,omitempty"`
	ContentType  string `json:"content_type"`
	Headers      map[string]string `json:"headers,omitempty"`
}
