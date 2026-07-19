// Command serve boots a real clawvisor-server subprocess for the Playwright
// browser lane. It mirrors e2e/testapp's config template and port-collision
// retry semantics — the two differences are that it serves the real built
// frontend from web/dist (so the browser lane tests the actual SPA) and that
// it runs as a standalone process Playwright's global-setup can spawn (testapp
// needs *testing.T, which a Playwright subprocess can't provide).
//
// Contract:
//   - `-port 0` allocates a free port; any other value is used verbatim.
//   - `-data-dir` defaults to a fresh temp dir (removed on exit).
//   - The server binary comes from $CLAWVISOR_BIN if set, else it is built
//     from ./cmd/clawvisor-server (same contract as `make test-e2e`).
//   - On readiness it prints ONE JSON line to stdout: {"url":...,"pid":...}
//     then blocks until SIGTERM/SIGINT, at which point it kills the child and
//     exits 0.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// maxStartAttempts caps retries on early-exit (the port-collision symptom),
// matching e2e/testapp/server.go. High enough to clear a transient port grab,
// low enough that a genuinely broken binary fails fast.
const maxStartAttempts = 4

func main() {
	port := flag.Int("port", 0, "port to bind (0 = allocate a free port)")
	dataDir := flag.String("data-dir", "", "server data dir (default: fresh temp dir)")
	flag.Parse()

	if err := run(*port, *dataDir); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func run(port int, dataDir string) error {
	binPath, err := resolveBinary()
	if err != nil {
		return fmt.Errorf("resolve server binary: %w", err)
	}
	frontendDir, err := resolveFrontendDir()
	if err != nil {
		return err
	}

	cleanupDataDir := false
	if dataDir == "" {
		dataDir, err = os.MkdirTemp("", "clawvisor-browser-lane-*")
		if err != nil {
			return fmt.Errorf("temp data dir: %w", err)
		}
		cleanupDataDir = true
	}
	if cleanupDataDir {
		defer os.RemoveAll(dataDir)
	}

	// Retry on early-exit only (the port-collision shape). A caller-fixed port
	// is not retried with a fresh one — we honor the requested port.
	var lastErr error
	for attempt := 0; attempt < maxStartAttempts; attempt++ {
		boundPort := port
		if boundPort == 0 {
			boundPort, err = freePort()
			if err != nil {
				return fmt.Errorf("alloc port: %w", err)
			}
		}
		child, exited, url, err := start(binPath, frontendDir, dataDir, boundPort)
		if err == nil {
			return serveUntilSignal(child, exited, url)
		}
		lastErr = err
		var early *earlyExitError
		if !errors.As(err, &early) || port != 0 {
			// Not a retryable shape, or the caller pinned the port (retrying
			// with the same port just collides again).
			return err
		}
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	return fmt.Errorf("server failed after %d attempts: %w", maxStartAttempts, lastErr)
}

// earlyExitError signals the subprocess died before /ready returned 200 — the
// symptom of a port collision (EADDRINUSE on bind kills the server at once).
type earlyExitError struct{ err error }

func (e *earlyExitError) Error() string { return "server exited before /ready: " + errString(e.err) }
func (e *earlyExitError) Unwrap() error { return e.err }

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// start writes the config, launches the server, and waits for /ready. On
// success it returns the child, an `exited` channel closed when the child's
// single cmd.Wait() completes, and the URL. On failure it tears down its own
// child inline and returns the error.
func start(binPath, frontendDir, dataDir string, port int) (*exec.Cmd, <-chan struct{}, string, error) {
	publicURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	vaultKeyFile := filepath.Join(dataDir, "vault.key")
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, nil, "", fmt.Errorf("rand: %w", err)
	}
	if err := os.WriteFile(vaultKeyFile, []byte(base64.StdEncoding.EncodeToString(keyBytes)), 0600); err != nil {
		return nil, nil, "", fmt.Errorf("write vault key: %w", err)
	}

	cfgPath := filepath.Join(dataDir, "config.yaml")
	cfg := fmt.Sprintf(`
server:
  port: %d
  host: "127.0.0.1"
  log_format: "text"
  log_level: "warn"
  public_url: "%s"
  frontend_dir: "%s"

database:
  driver: "sqlite"
  sqlite_path: "%s/clawvisor.db"

vault:
  backend: "local"
  local_key_file: "%s"

auth:
  jwt_secret: "browser-lane-jwt-secret-must-be-long-enough-32"
  access_token_ttl: "1h"
  refresh_token_ttl: "24h"

# The browser lane authenticates each spec's context via the magic-link flow,
# so the default 5/min per-IP auth limit would 429 the suite. A generous limit
# is appropriate for a single-tenant test server.
rate_limit:
  auth:
    limit: 10000
    window: 60

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
`, port, publicURL, frontendDir, dataDir, vaultKeyFile)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
		return nil, nil, "", fmt.Errorf("write config: %w", err)
	}

	cmd := exec.Command(binPath, "server")
	cmd.Env = append(os.Environ(),
		"CONFIG_FILE="+cfgPath,
		"CLAWVISOR_DATA_DIR="+dataDir,
		"LOG_LEVEL=warn",
	)
	// Own process group so we can signal the whole tree on teardown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = os.Stderr // surface startup failures to the caller's log

	if err := cmd.Start(); err != nil {
		return nil, nil, "", fmt.Errorf("start server: %w", err)
	}

	// The child's cmd.Wait() is called exactly once, here. serveUntilSignal
	// reuses this channel rather than calling Wait() a second time.
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()

	if err := waitReady(publicURL, 20*time.Second, exited); err != nil {
		_ = kill(cmd)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
		return nil, nil, "", err
	}
	return cmd, exited, publicURL, nil
}

func waitReady(baseURL string, timeout time.Duration, exited <-chan struct{}) error {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return &earlyExitError{err: errors.New("process exited")}
		default:
		}
		resp, err := client.Get(baseURL + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("server did not become ready within %s", timeout)
}

// serveUntilSignal prints the readiness line and blocks until a termination
// signal, then kills the child and returns nil (exit 0).
func serveUntilSignal(cmd *exec.Cmd, exited <-chan struct{}, url string) error {
	line, err := json.Marshal(struct {
		URL string `json:"url"`
		PID int    `json:"pid"`
	}{URL: url, PID: cmd.Process.Pid})
	if err != nil {
		return err
	}
	fmt.Println(string(line))
	os.Stdout.Sync()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	select {
	case <-sig:
	case <-exited:
		// Child died on its own — nothing left to serve.
		return nil
	}
	_ = kill(cmd)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
	return nil
}

// kill terminates the child's whole process group.
func kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Negative pid = the process group created via Setpgid.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// resolveBinary returns $CLAWVISOR_BIN if set, else builds the server from
// source into a temp path (same contract as make test-e2e / testserver_test).
func resolveBinary() (string, error) {
	if bin := os.Getenv("CLAWVISOR_BIN"); bin != "" {
		return bin, nil
	}
	root, err := projectRoot()
	if err != nil {
		return "", err
	}
	bin := filepath.Join(os.TempDir(), "clawvisor-browser-lane-server")
	// The binary name is constant ("go", resolved via PATH lookup) and the
	// package argument is a fixed in-repo path; only the output path varies,
	// and it is derived from os.TempDir + a constant name.
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", err
	}
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/clawvisor-server")
	build.Dir = root
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("build clawvisor-server: %w", err)
	}
	return bin, nil
}

// resolveFrontendDir returns the absolute path to the built frontend
// (web/dist). It must exist — the browser lane serves the real SPA.
func resolveFrontendDir() (string, error) {
	root, err := projectRoot()
	if err != nil {
		return "", err
	}
	dist := filepath.Join(root, "web", "dist")
	if _, err := os.Stat(filepath.Join(dist, "index.html")); err != nil {
		return "", fmt.Errorf("frontend not built at %s (run `npm run build` in web/ first): %w", dist, err)
	}
	return dist, nil
}

// projectRoot resolves the repo root relative to this program's working
// directory (e2e/browser/ when invoked as `go run ./serve`).
func projectRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// e2e/browser -> repo root is two levels up.
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", fmt.Errorf("could not locate repo root from %s: %w", wd, err)
	}
	return root, nil
}
