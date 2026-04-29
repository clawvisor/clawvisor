package proxy

import (
	"context"
	"testing"

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
