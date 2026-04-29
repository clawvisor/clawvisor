package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/internal/vault"
	"github.com/clawvisor/clawvisor/pkg/config"
)

var placeholderExtractRE = regexp.MustCompile(`autovault_[A-Za-z0-9._:-]+`)

func TestRuntimeSecretCaptureKnownPrefixAndReusePlaceholder(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-capture.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "capture-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	cfg := config.Default()
	cfg.LLM.Verification.Enabled = false
	hooks := InboundSecretHooks{Store: st, Vault: v, Config: cfg}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"use ghp_exampleSecret123456789 now"}]}]}`)

	first, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, hooks, runtimeSession, "api.anthropic.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets(first): %v", err)
	}
	if observed != 0 || summary == nil || summary.ReplacementCount != 1 {
		t.Fatalf("unexpected first summary: summary=%+v observed=%d", summary, observed)
	}
	firstPlaceholder := string(placeholderExtractRE.Find(first))
	if firstPlaceholder == "" || strings.Contains(string(first), "ghp_exampleSecret123456789") {
		t.Fatalf("expected rewritten placeholder body, got %s", string(first))
	}

	second, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, hooks, runtimeSession, "api.anthropic.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets(second): %v", err)
	}
	if observed != 0 || summary == nil || summary.ReplacementCount != 1 {
		t.Fatalf("unexpected second summary: summary=%+v observed=%d", summary, observed)
	}
	secondPlaceholder := string(placeholderExtractRE.Find(second))
	if secondPlaceholder != firstPlaceholder {
		t.Fatalf("expected placeholder reuse, got first=%q second=%q", firstPlaceholder, secondPlaceholder)
	}
	meta, err := st.GetRuntimePlaceholder(ctx, firstPlaceholder)
	if err != nil {
		t.Fatalf("GetRuntimePlaceholder: %v", err)
	}
	cred, err := v.Get(ctx, userID, meta.ServiceID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	if string(cred) != "ghp_exampleSecret123456789" {
		t.Fatalf("expected captured secret in vault, got %q", string(cred))
	}
}

func TestRuntimeSecretCaptureHeuristicAndPasswordReveal(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-heuristic.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "heuristic-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hooks := InboundSecretHooks{Store: st, Vault: v, Config: config.Default()}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"api_key=ZXhhbXBsZV9zZWNyZXRfdG9rZW5fMTIzNDU2Nzg5 and the password is hunter2secret99"}]}]}`)

	rewritten, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, hooks, runtimeSession, "api.openai.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if observed != 0 || summary == nil || summary.ReplacementCount < 2 {
		t.Fatalf("unexpected summary: summary=%+v observed=%d", summary, observed)
	}
	rewrittenBody := string(rewritten)
	if strings.Contains(rewrittenBody, "ZXhhbXBsZV9zZWNyZXRfdG9rZW5fMTIzNDU2Nzg5") || strings.Contains(rewrittenBody, "hunter2secret99") {
		t.Fatalf("expected both heuristic and password replacements, got %s", rewrittenBody)
	}
}

func TestRuntimeSecretCaptureObservesAmbiguousCandidateWithoutReplacing(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-observe.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "observe-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hooks := InboundSecretHooks{Store: st, Vault: v, Config: config.Default()}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"maybe use this id 123e4567-e89b-12d3-a456-426614174000 later"}]}]}`)

	rewritten, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, hooks, runtimeSession, "api.openai.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if strings.Contains(string(rewritten), "autovault_") {
		t.Fatalf("expected observe-only body without placeholders, got %s", string(rewritten))
	}
	if observed == 0 || summary == nil || summary.ReplacementCount != 0 {
		t.Fatalf("unexpected observe-only summary: summary=%+v observed=%d", summary, observed)
	}
}

func TestRuntimeSecretCapturePlaceholdersResolveThroughOutboundSwap(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-e2e.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "capture-swap-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"use ghp_bridgeSecret123456789 for github"}]}]}`)
	rewritten, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, InboundSecretHooks{Store: st, Vault: v, Config: config.Default()}, runtimeSession, "api.anthropic.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if observed != 0 || summary == nil || summary.ReplacementCount != 1 {
		t.Fatalf("unexpected capture summary: summary=%+v observed=%d", summary, observed)
	}
	placeholder := string(placeholderExtractRE.Find(rewritten))
	if placeholder == "" {
		t.Fatalf("expected placeholder in rewritten body: %s", string(rewritten))
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ghp_bridgeSecret123456789" {
			t.Fatalf("expected outbound placeholder swap, got %q", got)
		}
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	srv.InstallSessionGuard(&Authenticator{Store: st})
	srv.InstallPlaceholderSwap(PlaceholderHooks{Store: st, Vault: v})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(out) != "ok" {
		t.Fatalf("expected upstream success, got %d %q", resp.StatusCode, string(out))
	}
}
