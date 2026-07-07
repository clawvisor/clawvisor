package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"
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
