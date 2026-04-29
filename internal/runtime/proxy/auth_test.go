package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	intauth "github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestExtractBearerSecretAcceptsBearerAndBasic(t *testing.T) {
	t.Run("bearer", func(t *testing.T) {
		header := http.Header{}
		header.Set("Proxy-Authorization", "Bearer secret-token")
		secret, err := ExtractBearerSecret(header)
		if err != nil {
			t.Fatalf("ExtractBearerSecret: %v", err)
		}
		if secret != "secret-token" {
			t.Fatalf("unexpected secret %q", secret)
		}
	})

	t.Run("basic", func(t *testing.T) {
		header := http.Header{}
		creds := base64.StdEncoding.EncodeToString([]byte("clawvisor:secret-token"))
		header.Set("Proxy-Authorization", "Basic "+creds)
		secret, err := ExtractBearerSecret(header)
		if err != nil {
			t.Fatalf("ExtractBearerSecret: %v", err)
		}
		if secret != "secret-token" {
			t.Fatalf("unexpected secret %q", secret)
		}
	})
}

func TestProxyURLWithSecret(t *testing.T) {
	got, err := ProxyURLWithSecret("http://127.0.0.1:8080", "secret-token")
	if err != nil {
		t.Fatalf("ProxyURLWithSecret: %v", err)
	}
	if got != "http://clawvisor:secret-token@127.0.0.1:8080" {
		t.Fatalf("unexpected proxy URL %q", got)
	}
}

func TestAuthenticatorAcceptsAgentTokenAndCreatesReusableRuntimeSession(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/proxy-auth.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "proxy-auth@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rawToken := "cvis_runtime_agent_token"
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", intauth.HashToken(rawToken))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	cfg := config.Default()
	authn := &Authenticator{Store: st, Config: cfg}
	header := http.Header{}
	header.Set("Proxy-Authorization", "Bearer "+rawToken)

	first, err := authn.Authenticate(ctx, header)
	if err != nil {
		t.Fatalf("Authenticate(first): %v", err)
	}
	if first.AgentID != agent.ID || first.UserID != user.ID {
		t.Fatalf("unexpected runtime session attribution: %+v", first)
	}
	if !first.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("expected live runtime session, got expires_at=%s", first.ExpiresAt)
	}
	if !isAgentTokenRuntimeSession(first.MetadataJSON) {
		t.Fatalf("expected proxy-auth metadata, got %s", string(first.MetadataJSON))
	}

	second, err := authn.Authenticate(ctx, header)
	if err != nil {
		t.Fatalf("Authenticate(second): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected reusable runtime session, got first=%q second=%q", first.ID, second.ID)
	}

	sessions, err := st.ListRuntimeSessionsByAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("ListRuntimeSessionsByAgent: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected one server-managed runtime session, got %d", len(sessions))
	}
}

func TestAuthenticatorDoesNotReuseBootstrapRuntimeSessionsForAgentTokenAuth(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/proxy-auth-bootstrap.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "proxy-bootstrap@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rawToken := "cvis_runtime_agent_token_bootstrap"
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", intauth.HashToken(rawToken))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	bootstrapMetadata, _ := json.Marshal(map[string]any{
		"launcher": "clawvisor-agent-run",
	})
	if err := st.CreateRuntimeSession(ctx, &store.RuntimeSession{
		ID:                    "bootstrap-session",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: HashProxyBearerSecret("bootstrap-secret"),
		ObservationMode:       false,
		MetadataJSON:          bootstrapMetadata,
		ExpiresAt:             time.Now().UTC().Add(30 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateRuntimeSession(bootstrap): %v", err)
	}

	authn := &Authenticator{Store: st, Config: config.Default()}
	header := http.Header{}
	header.Set("Proxy-Authorization", "Bearer "+rawToken)

	session, err := authn.Authenticate(ctx, header)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if session.ID == "bootstrap-session" {
		t.Fatal("agent-token proxy auth should not reuse bootstrap runtime sessions")
	}

	sessions, err := st.ListRuntimeSessionsByAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("ListRuntimeSessionsByAgent: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected bootstrap + server-managed runtime session, got %d", len(sessions))
	}
}
