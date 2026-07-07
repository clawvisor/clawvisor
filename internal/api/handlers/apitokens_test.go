package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

// mintTokenReq drives the APITokensHandler.Create handler directly (the
// admin gate lives in middleware, so a handler-level test exercises only the
// mint-time scope validation).
func mintTokenReq(t *testing.T, h *APITokensHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/tokens", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	return rec
}

// TestAPITokenMintScopeValidation: after spec 04, the mint accepts exactly
// config-read, config-write, and instance-admin (empty defaults to
// instance-admin); any other value is 400 INVALID_SCOPE.
func TestAPITokenMintScopeValidation(t *testing.T) {
	st := flatTeamStore(t)
	h := NewAPITokensHandler(st, slog.Default())

	cases := []struct {
		name      string
		scope     string // "" omits the field entirely
		wantCode  int
		wantScope string // expected stored scope on success
	}{
		{"instance-admin", "instance-admin", http.StatusCreated, "instance-admin"},
		{"config-write", "config-write", http.StatusCreated, "config-write"},
		{"config-read", "config-read", http.StatusCreated, "config-read"},
		{"empty-defaults-admin", "", http.StatusCreated, "instance-admin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"name":"tok"}`
			if tc.scope != "" {
				body = `{"name":"tok","scope":"` + tc.scope + `"}`
			}
			rec := mintTokenReq(t, h, body)
			if rec.Code != tc.wantCode {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.wantCode, rec.Body.String())
			}
			var out struct {
				Token string `json:"token"`
				Scope string `json:"scope"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Token == "" {
				t.Fatal("mint returned no plaintext token")
			}
			if out.Scope != tc.wantScope {
				t.Fatalf("scope=%q want %q", out.Scope, tc.wantScope)
			}
		})
	}
}

// TestAPITokenMintRejectsInvalidScope: an unknown scope string is rejected at
// mint with 400 INVALID_SCOPE and no token is created.
func TestAPITokenMintRejectsInvalidScope(t *testing.T) {
	st := flatTeamStore(t)
	h := NewAPITokensHandler(st, slog.Default())

	for _, scope := range []string{"root", "admin", "config-admin", "instanceadmin"} {
		rec := mintTokenReq(t, h, `{"name":"tok","scope":"`+scope+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("scope %q: status=%d want 400 body=%s", scope, rec.Code, rec.Body.String())
		}
		if !bodyHasCode(rec.Body.String(), "INVALID_SCOPE") {
			t.Fatalf("scope %q: body missing INVALID_SCOPE: %s", scope, rec.Body.String())
		}
	}
}

// burnConflictStore wraps a real store but forces CreateAPITokenAndBurnBootstrap
// to lose the burn race: it returns store.ErrConflict, exactly as the real
// store does for the loser of a concurrent first-boot mint (the winner already
// revoked the single-use bootstrap token). Every other method delegates to the
// embedded store so the auth middleware still resolves the live bootstrap token.
type burnConflictStore struct {
	store.Store
}

func (burnConflictStore) CreateAPITokenAndBurnBootstrap(context.Context, *store.APIToken, string) error {
	return store.ErrConflict
}

// TestCreate_ConcurrentBootstrapBurnConflictReturns409 proves that when the
// burn loses the single-use race (store returns ErrConflict), the handler
// surfaces a retry-safe 409 CONFLICT rather than a 500 INTERNAL_ERROR.
func TestCreate_ConcurrentBootstrapBurnConflictReturns409(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "apitokens.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	real := sqlite.NewStore(db)

	// Seed a live bootstrap token so the auth middleware admits the request
	// and injects it (IsBootstrap) into the handler context.
	raw, prefix, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	if err := real.CreateAPIToken(ctx, &store.APIToken{
		Name:        "bootstrap",
		TokenHash:   auth.HashToken(raw),
		TokenPrefix: prefix,
		Scope:       middleware.ScopeInstanceAdmin,
		IsBootstrap: true,
	}); err != nil {
		t.Fatalf("seed bootstrap token: %v", err)
	}

	st := burnConflictStore{real}
	h := NewAPITokensHandler(st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	jwtSvc, err := auth.NewJWTService("test-secret-test-secret-test-secret-12345")
	if err != nil {
		t.Fatalf("NewJWTService: %v", err)
	}
	handler := middleware.RequireUserOrAPIToken(jwtSvc, st, middleware.ScopeInstanceAdmin)(http.HandlerFunc(h.Create))

	body, _ := json.Marshal(map[string]string{"name": "terraform", "scope": middleware.ScopeInstanceAdmin})
	req := httptest.NewRequest(http.MethodPost, "/api/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if resp.Code != "CONFLICT" {
		t.Fatalf("error code = %q, want CONFLICT; body=%q", resp.Code, rec.Body.String())
	}
}
