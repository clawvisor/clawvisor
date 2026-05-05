package proxy

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"log/slog"

	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/config"
)

func TestManagerCreateRuntimeSession(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	if _, err := st.CreateUser(ctx, "user-1@test.example", "hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	user, err := st.GetUserByEmail(ctx, "user-1@test.example")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()

	srv, err := NewServer(Config{
		DataDir: cfg.RuntimeProxy.DataDir,
		Addr:    "127.0.0.1:0",
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	manager := &Manager{
		Store:  st,
		Config: cfg,
		Proxy:  srv,
	}
	result, err := manager.CreateRuntimeSession(ctx, agent.ID, user.ID, CreateSessionRequest{})
	if err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if result.ProxyBearer == "" {
		t.Fatal("expected proxy bearer secret")
	}
	if result.ProxyURL == "" {
		t.Fatal("expected proxy URL")
	}
	if result.CACertPEM == "" {
		t.Fatal("expected CA cert PEM")
	}
	if result.Session.ProxyBearerSecretHash == result.ProxyBearer {
		t.Fatal("proxy bearer secret should not be stored in plaintext")
	}
}

func TestNewServerUsesVerifiedUpstreamTransport(t *testing.T) {
	srv, err := NewServer(Config{
		DataDir: t.TempDir(),
		Addr:    "127.0.0.1:0",
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.goproxy == nil || srv.goproxy.Tr == nil || srv.goproxy.Tr.TLSClientConfig == nil {
		t.Fatal("expected runtime proxy transport with TLS config")
	}
	if srv.goproxy.Tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("expected runtime proxy transport to verify upstream TLS")
	}
}

func TestNewServerRejectsInsecureAdjudicationDebugDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "adjudication-debug")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Setenv("CLAWVISOR_RUNTIME_PROXY_ADJUDICATION_DEBUG_DIR", dir)
	if _, err := NewServer(Config{
		DataDir: t.TempDir(),
		Addr:    "127.0.0.1:0",
	}, nil); err == nil {
		t.Fatal("expected insecure adjudication debug dir to be rejected")
	}
}

func TestNewServerWarnsWhenAdjudicationDebugDirEnabled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "adjudication-debug")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Setenv("CLAWVISOR_RUNTIME_PROXY_ADJUDICATION_DEBUG_DIR", dir)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := NewServer(Config{
		DataDir: t.TempDir(),
		Addr:    "127.0.0.1:0",
	}, logger); err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if !strings.Contains(logs.String(), "MUST NOT be set in production") {
		t.Fatalf("expected adjudication debug warning in logs, got %q", logs.String())
	}
}

func TestListenerTLSConfigPreMintsAllConfiguredHostnames(t *testing.T) {
	srv, err := NewServer(Config{
		DataDir:           t.TempDir(),
		Addr:              "127.0.0.1:0",
		ListenerHostnames: []string{"localhost", "127.0.0.1", "clawvisor.test"},
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if _, err := srv.listenerTLSConfig(); err != nil {
		t.Fatalf("listenerTLSConfig: %v", err)
	}
	for _, name := range []string{"localhost", "127.0.0.1", "clawvisor.test"} {
		if _, err := srv.certs.Get(name); err != nil {
			t.Fatalf("expected preminted cert for %s: %v", name, err)
		}
	}
}
