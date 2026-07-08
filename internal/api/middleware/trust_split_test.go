package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func jwtFor(t *testing.T, st store.Store, jwtSvc auth.TokenService, email, role string) string {
	t.Helper()
	u, err := st.CreateUser(context.Background(), email, "hash", role)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tok, err := jwtSvc.GenerateAccessToken(u.ID, u.Email, time.Hour)
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	return tok
}

func statusFor(h http.Handler, bearer string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestAPIToken_ScopeEnforcement pins the hierarchical scope model:
// instance-admin > config-write > config-read. A token satisfies a gate iff
// its scope ranks at or above the gate's minimum; otherwise 403
// INSUFFICIENT_SCOPE.
func TestAPIToken_ScopeEnforcement(t *testing.T) {
	st := newAPITokenTestStore(t)
	jwtSvc := newTestJWT(t)
	readTok := seedAPIToken(t, st, ScopeConfigRead, nil, false, false)
	writeTok := seedAPIToken(t, st, ScopeConfigWrite, nil, false, false)
	adminTok := seedAPIToken(t, st, ScopeInstanceAdmin, nil, false, false)

	readGate := RequireUserOrAPIToken(jwtSvc, st, ScopeConfigRead)(okHandler())
	writeGate := RequireUserOrAPIToken(jwtSvc, st, ScopeConfigWrite)(okHandler())
	adminGate := RequireUserOrAPIToken(jwtSvc, st, ScopeInstanceAdmin)(okHandler())

	cases := []struct {
		name       string
		gate       http.Handler
		token      string
		wantStatus int
	}{
		{"read-token on read-gate", readGate, readTok, http.StatusOK},
		{"read-token on write-gate", writeGate, readTok, http.StatusForbidden},
		{"read-token on admin-gate", adminGate, readTok, http.StatusForbidden},
		{"write-token on read-gate", readGate, writeTok, http.StatusOK},
		{"write-token on write-gate", writeGate, writeTok, http.StatusOK},
		{"write-token on admin-gate", adminGate, writeTok, http.StatusForbidden},
		{"admin-token on read-gate", readGate, adminTok, http.StatusOK},
		{"admin-token on write-gate", writeGate, adminTok, http.StatusOK},
		{"admin-token on admin-gate", adminGate, adminTok, http.StatusOK},
	}
	for _, tc := range cases {
		code, body := statusFor(tc.gate, tc.token)
		if code != tc.wantStatus {
			t.Errorf("%s: status=%d want %d body=%s", tc.name, code, tc.wantStatus, body)
		}
		if tc.wantStatus == http.StatusForbidden && !bodyHasCode(body, "INSUFFICIENT_SCOPE") {
			t.Errorf("%s: want INSUFFICIENT_SCOPE, body=%s", tc.name, body)
		}
	}
}

// TestAPIToken_TrustSplit pins the instance-administrative boundary
// (RequireAdminOrToken — used for user management, shared-vault writes,
// token management). It admits an instance-admin token or a JWT admin, and
// refuses a config-write token (INSUFFICIENT_SCOPE) and a member JWT
// (FORBIDDEN) — a compromised low-trust credential must not perform
// fleet-wide administrative acts.
func TestAPIToken_TrustSplit(t *testing.T) {
	st := newAPITokenTestStore(t)
	jwtSvc := newTestJWT(t)
	gate := RequireAdminOrToken(jwtSvc, st)(okHandler())

	adminTok := seedAPIToken(t, st, ScopeInstanceAdmin, nil, false, false)
	writeTok := seedAPIToken(t, st, ScopeConfigWrite, nil, false, false)
	adminJWT := jwtFor(t, st, jwtSvc, "admin@x", store.RoleAdmin)
	memberJWT := jwtFor(t, st, jwtSvc, "member@x", store.RoleMember)

	if code, body := statusFor(gate, adminTok); code != http.StatusOK {
		t.Errorf("instance-admin token: status=%d want 200 body=%s", code, body)
	}
	if code, body := statusFor(gate, writeTok); code != http.StatusForbidden || !bodyHasCode(body, "INSUFFICIENT_SCOPE") {
		t.Errorf("config-write token: status=%d body=%s want 403 INSUFFICIENT_SCOPE", code, body)
	}
	if code, body := statusFor(gate, adminJWT); code != http.StatusOK {
		t.Errorf("admin JWT: status=%d want 200 body=%s", code, body)
	}
	if code, body := statusFor(gate, memberJWT); code != http.StatusForbidden || !bodyHasCode(body, "FORBIDDEN") {
		t.Errorf("member JWT: status=%d body=%s want 403 FORBIDDEN", code, body)
	}
}

// TestRejectInstanceItemWriteByScopedToken pins the /api/vault/items write
// guard: a config-write API token authenticates as the `_instance` user, so
// letting it write a "personal" item would plant a fleet-wide shared entry
// the instance-admin /api/vault/shared surface reserves. The guard blocks it
// while leaving instance-admin tokens and JWT users untouched.
func TestRejectInstanceItemWriteByScopedToken(t *testing.T) {
	st := newAPITokenTestStore(t)
	jwtSvc := newTestJWT(t)

	// Wire the guard exactly as server.go does: config-write gate, then the
	// item-write guard, then the handler.
	gate := RequireUserOrAPIToken(jwtSvc, st, ScopeConfigWrite)(
		RejectInstanceItemWriteByScopedToken(okHandler()))

	writeTok := seedAPIToken(t, st, ScopeConfigWrite, nil, false, false)
	adminTok := seedAPIToken(t, st, ScopeInstanceAdmin, nil, false, false)
	memberJWT := jwtFor(t, st, jwtSvc, "member@x", store.RoleMember)

	if code, body := statusFor(gate, writeTok); code != http.StatusForbidden || !bodyHasCode(body, "FORBIDDEN") {
		t.Errorf("config-write token: status=%d body=%s want 403 FORBIDDEN (shared-entry plant blocked)", code, body)
	}
	if code, body := statusFor(gate, adminTok); code != http.StatusOK {
		t.Errorf("instance-admin token: status=%d want 200 body=%s", code, body)
	}
	if code, body := statusFor(gate, memberJWT); code != http.StatusOK {
		t.Errorf("member JWT (owns its own rows): status=%d want 200 body=%s", code, body)
	}
}
