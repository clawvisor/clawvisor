package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/expose"
)

func TestWriteAndReadExposePIDFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "expose.pid")
	if err := writeExposePIDFile(path); err != nil {
		t.Fatalf("writeExposePIDFile: %v", err)
	}
	got := readExposePIDFile(path)
	if got != os.Getpid() {
		t.Fatalf("readExposePIDFile = %d, want %d", got, os.Getpid())
	}
}

func TestReadExposePIDFileMissingReturnsZero(t *testing.T) {
	if got := readExposePIDFile(filepath.Join(t.TempDir(), "nope")); got != 0 {
		t.Fatalf("expected 0 for missing pidfile, got %d", got)
	}
}

func TestProxyExposeForegroundExitsOnCtxCancel(t *testing.T) {
	// Stand up two upstreams and exercise the foreground runner end-to-end so
	// we cover the readiness print, pidfile cleanup, and graceful ctx exit.
	t.Setenv("HOME", t.TempDir())

	proxyUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "proxy")
	}))
	defer proxyUp.Close()
	apiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "api")
	}))
	defer apiUp.Close()

	cfg := expose.Config{
		BindAddr:      "127.0.0.1",
		ProxyUpstream: strings.TrimPrefix(proxyUp.URL, "http://"),
		APIUpstream:   strings.TrimPrefix(apiUp.URL, "http://"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	doneCh := make(chan error, 1)
	go func() {
		defer wg.Done()
		doneCh <- runProxyExposeForeground(ctx, cfg)
	}()

	// Wait for pidfile to appear (proxy is "ready").
	pidPath, err := proxyExposePIDPath()
	if err != nil {
		t.Fatalf("proxyExposePIDPath: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(pidPath); statErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, statErr := os.Stat(pidPath); statErr != nil {
		t.Fatalf("pidfile %s never appeared: %v", pidPath, statErr)
	}

	cancel()
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("foreground returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("foreground did not exit on ctx cancel")
	}
	wg.Wait()

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("pidfile not cleaned up after exit: stat err=%v", err)
	}
}

func TestDefaultExposeUpstreamsHonorsLocalConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, ".clawvisor"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := `server:
  host: 10.0.0.5
  port: 18789
runtime_proxy:
  listen_addr: 127.0.0.1:33333
`
	if err := os.WriteFile(filepath.Join(tmp, ".clawvisor", "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	proxy, api, err := defaultExposeUpstreams()
	if err != nil {
		t.Fatalf("defaultExposeUpstreams: %v", err)
	}
	if proxy != "127.0.0.1:33333" {
		t.Errorf("proxy upstream: got %q want 127.0.0.1:33333", proxy)
	}
	if api != "10.0.0.5:18789" {
		t.Errorf("api upstream: got %q want 10.0.0.5:18789", api)
	}
}

func TestResolveExposeUpstreamsOverridesWithoutConfig(t *testing.T) {
	// Both overrides set → no config required.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	prevProxy, prevAPI := proxyExposeUpstreamProxy, proxyExposeUpstreamAPI
	t.Cleanup(func() {
		proxyExposeUpstreamProxy = prevProxy
		proxyExposeUpstreamAPI = prevAPI
	})
	proxyExposeUpstreamProxy = "10.1.2.3:4318"
	proxyExposeUpstreamAPI = "10.1.2.3:18789"

	proxy, api, err := resolveExposeUpstreams()
	if err != nil {
		t.Fatalf("resolveExposeUpstreams: %v", err)
	}
	if proxy != "10.1.2.3:4318" || api != "10.1.2.3:18789" {
		t.Fatalf("got proxy=%q api=%q", proxy, api)
	}
}

func TestResolveExposeUpstreamsOverridesProxyOnly(t *testing.T) {
	// Proxy override + config-derived API.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, ".clawvisor"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := `server:
  host: 10.0.0.5
  port: 18789
runtime_proxy:
  listen_addr: 127.0.0.1:25290
`
	if err := os.WriteFile(filepath.Join(tmp, ".clawvisor", "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	prevProxy, prevAPI := proxyExposeUpstreamProxy, proxyExposeUpstreamAPI
	t.Cleanup(func() {
		proxyExposeUpstreamProxy = prevProxy
		proxyExposeUpstreamAPI = prevAPI
	})
	proxyExposeUpstreamProxy = "127.0.0.1:4318"
	proxyExposeUpstreamAPI = ""

	proxy, api, err := resolveExposeUpstreams()
	if err != nil {
		t.Fatalf("resolveExposeUpstreams: %v", err)
	}
	if proxy != "127.0.0.1:4318" {
		t.Errorf("proxy: got %q want 127.0.0.1:4318 (override)", proxy)
	}
	if api != "10.0.0.5:18789" {
		t.Errorf("api: got %q want 10.0.0.5:18789 (config)", api)
	}
}

func TestResolveExposeUpstreamsRejectsBadOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	prevProxy, prevAPI := proxyExposeUpstreamProxy, proxyExposeUpstreamAPI
	t.Cleanup(func() {
		proxyExposeUpstreamProxy = prevProxy
		proxyExposeUpstreamAPI = prevAPI
	})

	cases := map[string][2]string{
		"missing port":  {"10.1.2.3", "10.1.2.3:18789"},
		"bad port":      {"10.1.2.3:abc", "10.1.2.3:18789"},
		"empty host":    {":4318", "10.1.2.3:18789"},
		"port too high": {"10.1.2.3:99999", "10.1.2.3:18789"},
	}
	for name, hp := range cases {
		t.Run(name, func(t *testing.T) {
			proxyExposeUpstreamProxy = hp[0]
			proxyExposeUpstreamAPI = hp[1]
			if _, _, err := resolveExposeUpstreams(); err == nil {
				t.Fatalf("expected error for %s (proxy=%q)", name, hp[0])
			}
		})
	}
}

// freePort returns an OS-assigned ephemeral port as a string, for tests that
// want to bind explicit ports without colliding.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port)
}

func TestProxyExposeForegroundOnlyWritesPidfileAfterReady(t *testing.T) {
	// Make pidfile presence the readiness contract: the foreground runner
	// must NOT write the pidfile until both listeners have bound. The
	// detached runner relies on this — pidfile presence == healthy.
	t.Setenv("HOME", t.TempDir())

	// Occupy a port so the proxy listener fails to bind. Run with explicit
	// ProxyPort = the occupied port so expose.Run returns an error before
	// onReady fires.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer occupied.Close()
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port

	cfg := expose.Config{
		BindAddr:      "127.0.0.1",
		ProxyPort:     occupiedPort,
		APIPort:       0,
		ProxyUpstream: "127.0.0.1:1",
		APIUpstream:   "127.0.0.1:2",
	}
	err = runProxyExposeForeground(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected runProxyExposeForeground to fail when port is occupied")
	}
	pidPath, perr := proxyExposePIDPath()
	if perr != nil {
		t.Fatalf("pidpath: %v", perr)
	}
	if _, statErr := os.Stat(pidPath); statErr == nil {
		t.Fatal("pidfile should not exist when bind fails — detached readiness contract is broken")
	}
}
