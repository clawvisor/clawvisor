package testapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/testharness"
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
func StartWith(t *testing.T, h *testharness.Harness, extraEnv map[string]string) *Server {
	t.Helper()

	binPath := buildServerBinary(t)
	port := freePort(t)
	publicURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	dataDir := t.TempDir()
	vaultKeyFile := filepath.Join(dataDir, "vault.key")
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(vaultKeyFile, []byte(base64.StdEncoding.EncodeToString(keyBytes)), 0600); err != nil {
		t.Fatalf("write vault key: %v", err)
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

proxy_lite:
  enabled: true
`, port, publicURL, dataDir, vaultKeyFile)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
		t.Fatalf("write config: %v", err)
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
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start clawvisor-server: %v", err)
	}

	s := &Server{
		t:       t,
		URL:     publicURL,
		DataDir: dataDir,
		Client:  &http.Client{Timeout: 10 * time.Second},
		cmd:     cmd,
		cancel:  cancel,
	}
	go drain(stdout, &s.stdoutMu, &s.stdout)
	go drain(stderr, &s.stdoutMu, &s.stderr)

	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
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

	if err := s.waitReady(20 * time.Second); err != nil {
		t.Fatalf("clawvisor not ready: %v", err)
	}
	return s
}

func (s *Server) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
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
