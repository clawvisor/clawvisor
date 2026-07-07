package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	intauth "github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/pkg/vault"
)

// llmCredFakeResolver is an in-process reference Resolver for the deterministic
// reference-mode test lane (mirrors pkg/vault's fakeResolver).
type llmCredFakeResolver struct {
	val []byte
	err error
}

func (f *llmCredFakeResolver) Resolve(context.Context, intvault.RefEnvelope) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.val, nil
}

// newLLMCredTestStore opens a sqlite store (which seeds the `_instance` system
// user + api_tokens table via migration 060) and returns it, the raw *sql.DB so
// a vault can share the same connection, and the temp dir for the vault key.
func newLLMCredTestStore(t *testing.T) (store.Store, *sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.New(ctx, filepath.Join(dir, "llmcred.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.NewStore(db), db, dir
}

func newLLMCredJWT(t *testing.T) auth.TokenService {
	t.Helper()
	jwtSvc, err := intauth.NewJWTService("test-secret-test-secret-test-secret-12345")
	if err != nil {
		t.Fatalf("NewJWTService: %v", err)
	}
	return jwtSvc
}

// seedLLMCredToken inserts an API token with the given scope and returns the
// raw plaintext bearer value.
func seedLLMCredToken(t *testing.T, st store.Store, scope string) string {
	t.Helper()
	raw, prefix, err := intauth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	tok := &store.APIToken{
		Name:        "llmcred-test",
		TokenHash:   intauth.HashToken(raw),
		TokenPrefix: prefix,
		Scope:       scope,
	}
	if err := st.CreateAPIToken(context.Background(), tok); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	return raw
}

// putLLMCred drives PUT /api/runtime/llm-credentials/{provider} through the
// write auth gate (instance-admin) with the given bearer token and JSON body.
func putLLMCred(t *testing.T, h *LLMCredentialsHandler, st store.Store, jwtSvc auth.TokenService, bearer, provider string, body any) *httptest.ResponseRecorder {
	t.Helper()
	gate := middleware.RequireUserOrAgentOrToken(jwtSvc, st, middleware.ScopeInstanceAdmin, true)
	handler := gate(http.HandlerFunc(h.Set))

	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/runtime/llm-credentials/"+provider, bytes.NewReader(raw))
	req.SetPathValue("provider", provider)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestLLMCredential_InstanceAdminTokenSetsSharedKey: a `cvat_` instance-admin
// token can PUT a provider key; it resolves to `_instance` so the key lands
// under the shared scope every agent falls back to.
func TestLLMCredential_InstanceAdminTokenSetsSharedKey(t *testing.T) {
	st, db, dir := newLLMCredTestStore(t)
	v, err := intvault.NewLocalVault(filepath.Join(dir, "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	h := NewLLMCredentialsHandler(st, v, nil)
	adminTok := seedLLMCredToken(t, st, middleware.ScopeInstanceAdmin)

	const key = "sk-ant-api03-shared-org-key"
	rec := putLLMCred(t, h, st, newLLMCredJWT(t), adminTok, "anthropic", map[string]any{"api_key": key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	got, err := v.Get(context.Background(), store.InstanceUserID, "anthropic")
	if err != nil {
		t.Fatalf("expected key stored under _instance: %v", err)
	}
	if string(got) != key {
		t.Fatalf("stored key=%q want %q", got, key)
	}
}

// TestLLMCredential_ConfigWriteTokenRefused: a config-write token — also
// injected as `_instance` — must NOT be able to set an instance-shared provider
// key; the write gate refuses it with 403 before the handler runs.
func TestLLMCredential_ConfigWriteTokenRefused(t *testing.T) {
	st, db, dir := newLLMCredTestStore(t)
	v, err := intvault.NewLocalVault(filepath.Join(dir, "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	h := NewLLMCredentialsHandler(st, v, nil)
	writeTok := seedLLMCredToken(t, st, middleware.ScopeConfigWrite)

	rec := putLLMCred(t, h, st, newLLMCredJWT(t), writeTok, "anthropic", map[string]any{"api_key": "sk-ant-api03-nope"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := v.Get(context.Background(), store.InstanceUserID, "anthropic"); err == nil {
		t.Fatal("config-write token planted a shared provider key; expected none")
	}
}

// TestLLMCredential_ReferenceModeStores: reference-mode PUT stores an
// external-secret reference under the provider service ID (honoring the
// allowlist — fail-closed when the target is not allowlisted).
func TestLLMCredential_ReferenceModeStores(t *testing.T) {
	st, db, dir := newLLMCredTestStore(t)
	crypto, err := intvault.NewLocalVault(filepath.Join(dir, "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	resolvers := map[string]intvault.Resolver{
		intvault.BackendAWSSM: &llmCredFakeResolver{val: []byte("resolved-anthropic-key")},
	}
	rv := intvault.NewReferenceVault(crypto, crypto, resolvers, []string{"arn:aws:secretsmanager:"})
	h := NewLLMCredentialsHandler(st, rv, nil)
	adminTok := seedLLMCredToken(t, st, middleware.ScopeInstanceAdmin)
	jwtSvc := newLLMCredJWT(t)

	// Allowlisted target → stored as a reference.
	rec := putLLMCred(t, h, st, jwtSvc, adminTok, "anthropic", map[string]any{
		"reference": map[string]any{
			"backend": intvault.BackendAWSSM,
			"id":      "arn:aws:secretsmanager:us-east-1:1:secret:anthropic-org",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reference store status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	kind, err := rv.EntryKind(context.Background(), store.InstanceUserID, "anthropic")
	if err != nil {
		t.Fatalf("EntryKind: %v", err)
	}
	if kind != intvault.KindRef {
		t.Fatalf("stored kind=%q want %q", kind, intvault.KindRef)
	}

	// Non-allowlisted target (not under the arn:aws:secretsmanager: prefix) →
	// fail-closed (no reference stored).
	rec = putLLMCred(t, h, st, jwtSvc, adminTok, "openai", map[string]any{
		"reference": map[string]any{
			"backend": intvault.BackendAWSSM,
			"id":      "arn:aws:s3:::some-bucket/not-a-secret",
		},
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("non-allowlisted reference should be refused; got 200 body=%s", rec.Body.String())
	}
	if k, _ := rv.EntryKind(context.Background(), store.InstanceUserID, "openai"); k == intvault.KindRef {
		t.Fatal("non-allowlisted reference was stored; expected fail-closed")
	}
}

// TestLLMCredential_ApiKeyXorReference: exactly one of api_key / reference is
// required — both or neither is a 400.
func TestLLMCredential_ApiKeyXorReference(t *testing.T) {
	st, db, dir := newLLMCredTestStore(t)
	v, err := intvault.NewLocalVault(filepath.Join(dir, "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	h := NewLLMCredentialsHandler(st, v, nil)
	adminTok := seedLLMCredToken(t, st, middleware.ScopeInstanceAdmin)
	jwtSvc := newLLMCredJWT(t)

	// Neither.
	if rec := putLLMCred(t, h, st, jwtSvc, adminTok, "anthropic", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("neither: status=%d want 400; body=%s", rec.Code, rec.Body.String())
	}
	// Both.
	rec := putLLMCred(t, h, st, jwtSvc, adminTok, "anthropic", map[string]any{
		"api_key":   "sk-ant-api03-key",
		"reference": map[string]any{"backend": intvault.BackendAWSSM, "id": "arn:aws:secretsmanager:x"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("both: status=%d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// OpenAI provider must reject Anthropic-shaped keys (sk-ant-…). Without
// the explicit exclusion, the broader sk-* prefix swallows them and
// the wrong key ends up in the openai vault entry.
func TestValidateLLMAPIKey_OpenAIRejectsAnthropicShape(t *testing.T) {
	if reason, ok := validateLLMAPIKey("openai", "sk-ant-this-is-anthropic"); ok {
		t.Fatalf("expected sk-ant-… to be rejected for openai; got ok=%v", ok)
	} else if reason == "" {
		t.Fatalf("expected a rejection reason")
	}
}

func TestValidateLLMAPIKey_OpenAIAcceptsRealOpenAIKeys(t *testing.T) {
	for _, k := range []string{
		"sk-proj-realopenairealkey",
		"sk-realopenairealkey12345",
	} {
		if _, ok := validateLLMAPIKey("openai", k); !ok {
			t.Errorf("expected valid openai key %q to pass", k)
		}
	}
}

func TestValidateLLMAPIKey_AnthropicRejectsOpenAIKey(t *testing.T) {
	if _, ok := validateLLMAPIKey("anthropic", "sk-proj-realopenairealkey"); ok {
		t.Fatalf("expected sk-proj-… to be rejected for anthropic")
	}
}
