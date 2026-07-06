package testapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/testharness"
)

// Server is a running clawvisor-server subprocess. Get URL+Client from
// the returned value and talk to it like any HTTP service. Cleanup is
// automatic via t.Cleanup.
type Server struct {
	t        *testing.T
	URL      string
	DataDir  string
	Client   *http.Client
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	stdoutMu sync.Mutex
	stdout   []byte
	stderr   []byte
	drains   sync.WaitGroup // drain goroutines; Wait() returns once pipes hit EOF
}

// Stderr returns the subprocess stderr captured so far. Config-load advisories
// (e.g. the pre-flip config_schema notice, spec 08) are emitted here at
// startup via the default slog handler before the server installs its own
// leveled logger, so they appear regardless of the configured log level.
func (s *Server) Stderr() string {
	s.stdoutMu.Lock()
	defer s.stdoutMu.Unlock()
	return string(s.stderr)
}

// Start builds + boots clawvisor-server on a free port, wires it to the
// provided Harness via env-var overrides, waits for /ready, and returns.
// The subprocess is killed via t.Cleanup; if the test failed, its
// stdout/stderr are dumped via t.Logf for diagnosis.
func Start(t *testing.T, h *testharness.Harness) *Server {
	return StartWith(t, h, nil)
}

// StartWith is like Start with additional env overrides. Use this for
// tests that need CLAWVISOR_LLM_UPSTREAM_* pointed at cassette servers
// or other per-test environment tweaks.
//
// freePort hands out a port by listening on :0 then closing — there's a
// small window where another process can grab it before clawvisor binds
// (classic TOCTOU; the common flaky-test source under CI parallelism).
// We detect the symptom (subprocess exits before /ready returns 200)
// and retry up to maxStartAttempts times with a fresh port. Slow-start
// timeouts (no early exit) are NOT retried — they'd just timeout again.
func StartWith(t *testing.T, h *testharness.Harness, extraEnv map[string]string) *Server {
	return StartWithConfig(t, h, extraEnv, "")
}

// StartWithConfig is like StartWith but appends configOverlay (raw YAML,
// top-level keys) to the generated config file before boot. Use it for
// file-only config surfaces that have no env override — e.g. the
// `posture:` preset key (spec 02), which is read from the config file
// only. The overlay is appended after the base blocks, so it may set new
// top-level keys or rely on preset semantics for absent sub-knobs.
func StartWithConfig(t *testing.T, h *testharness.Harness, extraEnv map[string]string, configOverlay string) *Server {
	return startWith(t, h, startOpts{extraEnv: extraEnv, overlay: configOverlay})
}

// StartWithoutProxyLite boots a server whose config file has NO proxy_lite
// section at all — byte-for-byte the shape a pre-flip wizard produced. Used by
// the flip's pre-flip-config scenario (spec 08) to prove absent-key ==
// disabled by construction and that Load()'s advisory config_schema notice
// fires. All other blocks (server/db/vault/auth) match the standard fixture.
func StartWithoutProxyLite(t *testing.T, h *testharness.Harness, extraEnv map[string]string) *Server {
	return startWith(t, h, startOpts{extraEnv: extraEnv, omitProxyLite: true})
}

// startOpts carries the per-boot knobs that vary between the public Start*
// helpers. Kept internal so the public surface stays additive.
type startOpts struct {
	extraEnv      map[string]string
	overlay       string
	omitProxyLite bool
}

func startWith(t *testing.T, h *testharness.Harness, opts startOpts) *Server {
	t.Helper()
	binPath := buildServerBinary(t)
	var lastErr error
	for attempt := 0; attempt < maxStartAttempts; attempt++ {
		s, err := tryStart(t, h, binPath, opts)
		if err == nil {
			return s
		}
		lastErr = err
		var earlyExit *earlyExitError
		if !errors.As(err, &earlyExit) {
			// Not a retryable shape (e.g. /ready timeout, config write
			// failure). Surfacing immediately avoids a 4×20s test stall.
			t.Fatalf("clawvisor-server start: %v", err)
		}
		// Brief backoff so the colliding process can settle / be torn down.
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	t.Fatalf("clawvisor-server failed after %d attempts (port-collision retries exhausted): %v", maxStartAttempts, lastErr)
	return nil
}

// maxStartAttempts caps retry on early-exit. Set above the realistic
// collision rate so a transient port grab clears, but low enough that a
// genuinely broken binary fails fast.
const maxStartAttempts = 4

// earlyExitError is returned by tryStart when the subprocess exited
// before /ready returned 200 — the symptom shape of a port collision
// (EADDRINUSE on bind kills the server immediately) or other startup
// failure. StartWith uses errors.As to decide whether to retry.
//
// stdout/stderr capture the subprocess's startup output, attached by
// tryStart after the drain goroutines have flushed. Without this,
// retries discard the actual "bind: address already in use" line and
// the final t.Fatalf only surfaces "exit status 1".
type earlyExitError struct {
	err    error
	stdout string
	stderr string
}

func (e *earlyExitError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "clawvisor exited before /ready: %v", e.err)
	if e.stdout != "" {
		fmt.Fprintf(&b, "\n--- subprocess stdout ---\n%s", e.stdout)
	}
	if e.stderr != "" {
		fmt.Fprintf(&b, "\n--- subprocess stderr ---\n%s", e.stderr)
	}
	return b.String()
}

func (e *earlyExitError) Unwrap() error { return e.err }

// tryStart performs one boot attempt. On success: registers t.Cleanup
// and returns the *Server. On failure: tears down its own subprocess
// inline and returns the error (no cleanup registered, so retries
// don't accumulate cleanup callbacks).
func tryStart(t *testing.T, h *testharness.Harness, binPath string, opts startOpts) (*Server, error) {
	t.Helper()
	port := freePort(t)
	publicURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	dataDir := t.TempDir()
	vaultKeyFile := filepath.Join(dataDir, "vault.key")
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, fmt.Errorf("rand: %w", err)
	}
	if err := os.WriteFile(vaultKeyFile, []byte(base64.StdEncoding.EncodeToString(keyBytes)), 0600); err != nil {
		return nil, fmt.Errorf("write vault key: %w", err)
	}

	cfgPath := filepath.Join(dataDir, "config.yaml")
	cfg := fmt.Sprintf(`
server:
  port: %d
  host: "127.0.0.1"
  log_format: "text"
  log_level: "warn"
  public_url: "%s"

database:
  driver: "sqlite"
  sqlite_path: "%s/clawvisor.db"

vault:
  backend: "local"
  local_key_file: "%s"

auth:
  jwt_secret: "test-jwt-secret-must-be-long-enough-32"
  access_token_ttl: "1h"
  refresh_token_ttl: "24h"

approval:
  timeout: 60
  on_timeout: "fail"

task:
  default_expiry_seconds: 1800

relay:
  enabled: false

push:
  enabled: false

telemetry:
  enabled: false

runtime_proxy:
  enabled: false
`, port, publicURL, dataDir, vaultKeyFile)
	// proxy_lite is enabled by default in the fixture; the pre-flip scenario
	// omits the block entirely to prove absent-key == disabled (spec 08).
	if !opts.omitProxyLite {
		cfg += "\nproxy_lite:\n  enabled: true\n"
	}
	if opts.overlay != "" {
		cfg += "\n" + opts.overlay + "\n"
	}
	if err := os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binPath, "server")
	env := os.Environ()
	env = append(env,
		"CONFIG_FILE="+cfgPath,
		"CLAWVISOR_DATA_DIR="+dataDir,
		"LOG_LEVEL=warn",
	)
	for k, v := range h.Env() {
		env = append(env, k+"="+v)
	}
	for k, v := range opts.extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start clawvisor-server: %w", err)
	}

	s := &Server{
		t:       t,
		URL:     publicURL,
		DataDir: dataDir,
		Client:  &http.Client{Timeout: 10 * time.Second},
		cmd:     cmd,
		cancel:  cancel,
	}
	s.drains.Add(2)
	go func() { defer s.drains.Done(); drain(stdout, &s.stdoutMu, &s.stdout) }()
	go func() { defer s.drains.Done(); drain(stderr, &s.stdoutMu, &s.stderr) }()

	// cmd.Wait can only be called once. Run it here so both the readiness
	// loop (early-exit detection) and t.Cleanup (orderly teardown) can
	// observe completion. Closed-channel signal so multiple receivers
	// can wait on it without one of them consuming the value.
	exited := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		close(exited)
	}()

	if err := s.waitReady(20*time.Second, exited, &exitErr); err != nil {
		cancel()
		// Bound the post-cancel wait so SIGKILL latency doesn't stall.
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
		// Wait for drain goroutines to flush so the captured buffers
		// include the final stderr lines (the actual EADDRINUSE / config
		// error / panic). Bounded — pipes should EOF immediately after
		// the process exits; the timeout is a backstop, not the path.
		s.waitDrains(500 * time.Millisecond)
		// Attach the captured output to the error so retries don't lose
		// it. StartWith's final t.Fatalf prints the last attempt's error,
		// which now carries the diagnostics.
		if eee, ok := err.(*earlyExitError); ok {
			s.stdoutMu.Lock()
			eee.stdout = string(s.stdout)
			eee.stderr = string(s.stderr)
			s.stdoutMu.Unlock()
		}
		return nil, err
	}

	// Register cleanup ONLY on success. On retry, the failed attempt's
	// cleanup must NOT fire after the eventual successful attempt — the
	// inline drain above already tore it down.
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
		s.waitDrains(500 * time.Millisecond)
		if t.Failed() {
			s.stdoutMu.Lock()
			defer s.stdoutMu.Unlock()
			if len(s.stdout) > 0 {
				t.Logf("=== clawvisor stdout ===\n%s", s.stdout)
			}
			if len(s.stderr) > 0 {
				t.Logf("=== clawvisor stderr ===\n%s", s.stderr)
			}
		}
	})

	return s, nil
}

// waitReady polls /ready until 200, or returns an earlyExitError if the
// subprocess died first (which is what a port collision looks like:
// EADDRINUSE on bind → fatal log → exit), or a plain error on timeout.
// StartWith retries on earlyExitError but not on timeout.
//
// exited is a signal channel (closed when cmd.Wait returns); exitErrPtr
// points to the captured Wait error written by the same goroutine. Both
// are safe to read after exited closes due to the happens-before chain.
func (s *Server) waitReady(timeout time.Duration, exited <-chan struct{}, exitErrPtr *error) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return &earlyExitError{err: *exitErrPtr}
		default:
		}
		resp, err := s.Client.Get(s.URL + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("clawvisor did not become ready within %s", timeout)
}

// waitDrains blocks until both drain goroutines have returned (pipes
// reached EOF), or the timeout elapses. Used on the error path so the
// captured stdout/stderr are flushed before being read into the
// returned earlyExitError. Bounded because a hung drain shouldn't stall
// the retry loop — partial output beats no output.
func (s *Server) waitDrains(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		s.drains.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("alloc port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func drain(r io.Reader, mu *sync.Mutex, dst *[]byte) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			mu.Lock()
			*dst = append(*dst, buf[:n]...)
			mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// jsonDecode and bytesReader are small helpers used by the fixture file
// — defined here to keep fixture.go's imports tight.
func jsonDecode(r io.Reader, dst any) error { return json.NewDecoder(r).Decode(dst) }
func bytesReader(b []byte) *bytes.Reader    { return bytes.NewReader(b) }
