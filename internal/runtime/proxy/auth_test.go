package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	intauth "github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type authStoreWrapper struct {
	store.Store
	getRuntimeSessionBySecretHashErr error
	getAgentByTokenErr               error
	listRuntimeSessionsByAgentErr    error
	updateRuntimeSessionExpiryErr    error
}

func (w authStoreWrapper) GetRuntimeSessionByProxyBearerSecretHash(ctx context.Context, hash string) (*store.RuntimeSession, error) {
	if w.getRuntimeSessionBySecretHashErr != nil {
		return nil, w.getRuntimeSessionBySecretHashErr
	}
	return w.Store.GetRuntimeSessionByProxyBearerSecretHash(ctx, hash)
}

func (w authStoreWrapper) GetAgentByToken(ctx context.Context, tokenHash string) (*store.Agent, error) {
	if w.getAgentByTokenErr != nil {
		return nil, w.getAgentByTokenErr
	}
	return w.Store.GetAgentByToken(ctx, tokenHash)
}

func (w authStoreWrapper) ListRuntimeSessionsByAgent(ctx context.Context, agentID string) ([]*store.RuntimeSession, error) {
	if w.listRuntimeSessionsByAgentErr != nil {
		return nil, w.listRuntimeSessionsByAgentErr
	}
	return w.Store.ListRuntimeSessionsByAgent(ctx, agentID)
}

func (w authStoreWrapper) UpdateRuntimeSessionExpiry(ctx context.Context, id string, expiresAt time.Time) error {
	if w.updateRuntimeSessionExpiryErr != nil {
		return w.updateRuntimeSessionExpiryErr
	}
	return w.Store.UpdateRuntimeSessionExpiry(ctx, id, expiresAt)
}

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

func TestExtractBearerCredentialsParsesUsername(t *testing.T) {
	header := http.Header{}
	creds := base64.StdEncoding.EncodeToString([]byte("launch-abc123:secret-token"))
	header.Set("Proxy-Authorization", "Basic "+creds)
	user, secret, err := ExtractBearerCredentials(header)
	if err != nil {
		t.Fatalf("ExtractBearerCredentials: %v", err)
	}
	if user != "launch-abc123" {
		t.Fatalf("unexpected username %q", user)
	}
	if secret != "secret-token" {
		t.Fatalf("unexpected secret %q", secret)
	}
}

func TestParseLaunchID(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"clawvisor":            "",
		"launch-":              "",
		"launch-abc123":        "abc123",
		"launch-deadbeef-uuid": "deadbeef-uuid",
	}
	for input, want := range cases {
		if got := ParseLaunchID(input); got != want {
			t.Fatalf("ParseLaunchID(%q) = %q, want %q", input, got, want)
		}
	}
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

func TestAuthenticatorMintsDistinctSessionsPerLaunchID(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/proxy-auth-launch.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "proxy-auth-launch@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rawToken := "cvis_runtime_agent_token_launch"
	if _, err := st.CreateAgent(ctx, user.ID, "runtime-agent", intauth.HashToken(rawToken)); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	authn := &Authenticator{Store: st, Config: config.Default()}

	headerWithLaunch := func(launch string) http.Header {
		h := http.Header{}
		creds := base64.StdEncoding.EncodeToString([]byte(launch + ":" + rawToken))
		h.Set("Proxy-Authorization", "Basic "+creds)
		return h
	}

	a1, err := authn.Authenticate(ctx, headerWithLaunch("launch-aaa"))
	if err != nil {
		t.Fatalf("Authenticate(a1): %v", err)
	}
	a2, err := authn.Authenticate(ctx, headerWithLaunch("launch-aaa"))
	if err != nil {
		t.Fatalf("Authenticate(a2): %v", err)
	}
	if a1.ID != a2.ID {
		t.Fatalf("expected reuse for same launch id, got %q vs %q", a1.ID, a2.ID)
	}
	b, err := authn.Authenticate(ctx, headerWithLaunch("launch-bbb"))
	if err != nil {
		t.Fatalf("Authenticate(b): %v", err)
	}
	if b.ID == a1.ID {
		t.Fatalf("expected distinct session for different launch id, got %q", b.ID)
	}
}

func TestAuthenticatorExtendsExpiryWhenSessionIsActivelyUsed(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/proxy-auth-extend.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "proxy-auth-extend@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rawToken := "cvis_runtime_agent_token_extend"
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", intauth.HashToken(rawToken))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	authn := &Authenticator{Store: st, Config: config.Default()}
	header := http.Header{}
	header.Set("Proxy-Authorization", "Bearer "+rawToken)

	first, err := authn.Authenticate(ctx, header)
	if err != nil {
		t.Fatalf("Authenticate(first): %v", err)
	}

	// Push the session close to expiry to simulate an actively-used long-lived
	// container. With the default 30-minute TTL, anything under 15 minutes
	// remaining should trigger a sliding extension on the next auth.
	near := time.Now().UTC().Add(2 * time.Minute)
	if err := st.UpdateRuntimeSessionExpiry(ctx, first.ID, near); err != nil {
		t.Fatalf("UpdateRuntimeSessionExpiry: %v", err)
	}

	second, err := authn.Authenticate(ctx, header)
	if err != nil {
		t.Fatalf("Authenticate(second): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected reuse with sliding TTL, got %q vs %q", first.ID, second.ID)
	}
	if !second.ExpiresAt.After(near.Add(5 * time.Minute)) {
		t.Fatalf("expected expires_at to slide forward, got %s (was %s)", second.ExpiresAt, near)
	}

	sessions, err := st.ListRuntimeSessionsByAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("ListRuntimeSessionsByAgent: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected single agent-token session after extension, got %d", len(sessions))
	}
}

func TestAuthenticatorReturnsUnavailableOnStoreErrors(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/proxy-auth-unavailable.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	baseStore := sqlite.NewStore(db)

	user, err := baseStore.CreateUser(ctx, "proxy-auth-unavailable@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rawToken := "cvis_runtime_agent_token_unavailable"
	agent, err := baseStore.CreateAgent(ctx, user.ID, "runtime-agent", intauth.HashToken(rawToken))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	header := http.Header{}
	header.Set("Proxy-Authorization", "Bearer "+rawToken)

	t.Run("agent lookup failure", func(t *testing.T) {
		authn := &Authenticator{
			Store: authStoreWrapper{
				Store:                            baseStore,
				getRuntimeSessionBySecretHashErr: store.ErrNotFound,
				getAgentByTokenErr:               errors.New("db unavailable"),
			},
			Config: config.Default(),
		}
		_, err := authn.Authenticate(ctx, header)
		if !errors.Is(err, ErrProxyAuthorizationUnavailable) {
			t.Fatalf("Authenticate error = %v, want ErrProxyAuthorizationUnavailable", err)
		}
	})

	t.Run("session listing failure", func(t *testing.T) {
		authn := &Authenticator{
			Store: authStoreWrapper{
				Store:                         baseStore,
				listRuntimeSessionsByAgentErr: errors.New("db unavailable"),
			},
			Config: config.Default(),
		}
		_, err := authn.Authenticate(ctx, header)
		if !errors.Is(err, ErrProxyAuthorizationUnavailable) {
			t.Fatalf("Authenticate error = %v, want ErrProxyAuthorizationUnavailable", err)
		}
	})

	t.Run("runtime session secret lookup failure", func(t *testing.T) {
		runtimeSession := &store.RuntimeSession{
			ID:                    "runtime-secret-session",
			UserID:                user.ID,
			AgentID:               agent.ID,
			Mode:                  "proxy",
			ProxyBearerSecretHash: HashProxyBearerSecret("session-secret"),
			ExpiresAt:             time.Now().UTC().Add(30 * time.Minute),
		}
		if err := baseStore.CreateRuntimeSession(ctx, runtimeSession); err != nil {
			t.Fatalf("CreateRuntimeSession: %v", err)
		}
		authn := &Authenticator{
			Store: authStoreWrapper{
				Store:                            baseStore,
				getRuntimeSessionBySecretHashErr: errors.New("db unavailable"),
			},
			Config: config.Default(),
		}
		sessionHeader := http.Header{}
		sessionHeader.Set("Proxy-Authorization", "Bearer session-secret")
		_, err := authn.Authenticate(ctx, sessionHeader)
		if !errors.Is(err, ErrProxyAuthorizationUnavailable) {
			t.Fatalf("Authenticate error = %v, want ErrProxyAuthorizationUnavailable", err)
		}
	})
}
