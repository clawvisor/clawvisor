package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	intauth "github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func newAPITokenTestStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "apitoken.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.NewStore(db)
}

// seedAPIToken inserts a token with the given hash/scope and optional
// expiry/revocation, returning the raw plaintext value.
func seedAPIToken(t *testing.T, st store.Store, scope string, expiresAt *time.Time, revoked bool, bootstrap bool) string {
	t.Helper()
	ctx := context.Background()
	raw, prefix, err := intauth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	tok := &store.APIToken{
		Name:        "test-token",
		TokenHash:   intauth.HashToken(raw),
		TokenPrefix: prefix,
		Scope:       scope,
		ExpiresAt:   expiresAt,
		IsBootstrap: bootstrap,
	}
	if err := st.CreateAPIToken(ctx, tok); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if revoked {
		if err := st.RevokeAPIToken(ctx, tok.ID); err != nil {
			t.Fatalf("RevokeAPIToken: %v", err)
		}
	}
	return raw
}

type apiTokenCapture struct {
	User  *store.User
	Token *store.APIToken
}

func (c *apiTokenCapture) handler(w http.ResponseWriter, r *http.Request) {
	c.User = UserFromContext(r.Context())
	c.Token = APITokenFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
}

func newTestJWT(t *testing.T) auth.TokenService {
	t.Helper()
	jwtSvc, err := intauth.NewJWTService("test-secret-test-secret-test-secret-12345")
	if err != nil {
		t.Fatalf("NewJWTService: %v", err)
	}
	return jwtSvc
}

func TestScopeSatisfies(t *testing.T) {
	cases := []struct {
		token, min string
		want       bool
	}{
		{ScopeInstanceAdmin, ScopeInstanceAdmin, true},
		{ScopeInstanceAdmin, ScopeConfigWrite, true},
		{ScopeInstanceAdmin, ScopeConfigRead, true},
		{ScopeConfigWrite, ScopeInstanceAdmin, false},
		{ScopeConfigWrite, ScopeConfigWrite, true},
		{ScopeConfigRead, ScopeConfigWrite, false},
		{ScopeConfigRead, ScopeConfigRead, true},
		{"", ScopeConfigRead, false},
		{ScopeConfigRead, "bogus", false},
	}
	for _, tc := range cases {
		if got := ScopeSatisfies(tc.token, tc.min); got != tc.want {
			t.Errorf("ScopeSatisfies(%q,%q)=%v want %v", tc.token, tc.min, got, tc.want)
		}
	}
}

func TestRequireUserOrAPIToken_ValidToken(t *testing.T) {
	st := newAPITokenTestStore(t)
	raw := seedAPIToken(t, st, ScopeInstanceAdmin, nil, false, false)

	cap := &apiTokenCapture{}
	h := RequireUserOrAPIToken(newTestJWT(t), st, ScopeInstanceAdmin, true)(http.HandlerFunc(cap.handler))

	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	// Attach a LogFields accumulator so we can assert the principal fields.
	ctx, lf := WithLogFields(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if cap.User == nil || cap.User.ID != store.InstanceUserID {
		t.Fatalf("expected _instance user injected, got %+v", cap.User)
	}
	if cap.Token == nil || cap.Token.Scope != ScopeInstanceAdmin {
		t.Fatalf("expected token in context, got %+v", cap.Token)
	}
	// Principal fields are the token, not _instance.
	var hasTokenID, hasUserID bool
	for _, a := range lf.Attrs() {
		switch a.Key {
		case "token_id":
			hasTokenID = true
		case "user_id":
			hasUserID = true
		}
	}
	if !hasTokenID {
		t.Fatal("expected token_id log field (principal attribution)")
	}
	if hasUserID {
		t.Fatal("did not expect user_id log field for token auth (principal must be the token)")
	}
}

func TestRequireUserOrAPIToken_Revoked(t *testing.T) {
	st := newAPITokenTestStore(t)
	raw := seedAPIToken(t, st, ScopeInstanceAdmin, nil, true, false)
	h := RequireUserOrAPIToken(newTestJWT(t), st, ScopeInstanceAdmin, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !bodyHasCode(rec.Body.String(), "TOKEN_REVOKED") {
		t.Fatalf("want TOKEN_REVOKED, body=%q", rec.Body.String())
	}
}

func TestRequireUserOrAPIToken_Expired(t *testing.T) {
	st := newAPITokenTestStore(t)
	past := time.Now().Add(-time.Hour)
	raw := seedAPIToken(t, st, ScopeInstanceAdmin, &past, false, false)
	h := RequireUserOrAPIToken(newTestJWT(t), st, ScopeInstanceAdmin, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !bodyHasCode(rec.Body.String(), "TOKEN_EXPIRED") {
		t.Fatalf("status=%d body=%q want 401 TOKEN_EXPIRED", rec.Code, rec.Body.String())
	}
}

func TestRequireUserOrAPIToken_InsufficientScope(t *testing.T) {
	st := newAPITokenTestStore(t)
	// A config-read token cannot satisfy an instance-admin gate.
	raw := seedAPIToken(t, st, ScopeConfigRead, nil, false, false)
	h := RequireUserOrAPIToken(newTestJWT(t), st, ScopeInstanceAdmin, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest(http.MethodPost, "/api/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !bodyHasCode(rec.Body.String(), "INSUFFICIENT_SCOPE") {
		t.Fatalf("status=%d body=%q want 403 INSUFFICIENT_SCOPE", rec.Code, rec.Body.String())
	}
}

func TestRequireUserOrAPIToken_UnknownToken(t *testing.T) {
	st := newAPITokenTestStore(t)
	raw, _, _ := intauth.GenerateAPIToken() // never inserted
	h := RequireUserOrAPIToken(newTestJWT(t), st, ScopeInstanceAdmin, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
}

// TestRequireUserOrAPIToken_JWTFallthrough asserts a non-cvat_ bearer is
// handed to the JWT path unchanged (regression guard). No cvat_ prefix →
// invalid JWT → 401 from RequireUser, never touching the token path.
func TestRequireUserOrAPIToken_JWTFallthrough(t *testing.T) {
	st := newAPITokenTestStore(t)
	h := RequireUserOrAPIToken(newTestJWT(t), st, ScopeInstanceAdmin, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	// A bogus non-cvat_ bearer should reach RequireUser and be rejected as
	// an invalid JWT (UNAUTHORIZED), proving fallthrough.
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer not-a-cvat-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 (JWT path)", rec.Code)
	}
}

// TestRequireUserOrAPIToken_FailsClosedWithoutInstanceUser proves the
// middleware 500s (never silently attributes to a random user) when the
// `_instance` row is absent. We delete the seeded row to simulate a
// missing seed.
func TestRequireUserOrAPIToken_FailsClosedWithoutInstanceUser(t *testing.T) {
	st := newAPITokenTestStore(t)
	raw := seedAPIToken(t, st, ScopeInstanceAdmin, nil, false, false)
	if err := st.DeleteUser(context.Background(), store.InstanceUserID); err != nil {
		t.Fatalf("DeleteUser(_instance): %v", err)
	}
	h := RequireUserOrAPIToken(newTestJWT(t), st, ScopeInstanceAdmin, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 (fail closed)", rec.Code)
	}
}

// TestBootstrap_Expires: a bootstrap token past its (mandatory +24h)
// expiry is rejected with 401 TOKEN_EXPIRED just like any other expired
// token — the 24h hard expiry is enforced regardless of is_bootstrap.
func TestBootstrap_Expires(t *testing.T) {
	st := newAPITokenTestStore(t)
	past := time.Now().Add(-time.Minute)
	raw := seedAPIToken(t, st, ScopeInstanceAdmin, &past, false, true /* bootstrap */)
	h := RequireUserOrAPIToken(newTestJWT(t), st, ScopeInstanceAdmin, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest(http.MethodPost, "/api/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !bodyHasCode(rec.Body.String(), "TOKEN_EXPIRED") {
		t.Fatalf("status=%d body=%q want 401 TOKEN_EXPIRED", rec.Code, rec.Body.String())
	}
}

// TestAPIToken_DisabledRejectsValidToken pins the key hardening: when API
// tokens are disabled instance-wide (apiTokensEnabled=false), a VALID seeded
// instance-admin cvat_ token on an adminOrToken route is rejected 401 WITHOUT
// a token-table lookup — a leaked / DB-planted token is inert. A normal admin
// JWT on the same gate still succeeds (200).
func TestAPIToken_DisabledRejectsValidToken(t *testing.T) {
	st := newAPITokenTestStore(t)
	jwtSvc := newTestJWT(t)

	// A perfectly valid, non-revoked, non-expired instance-admin token.
	adminTok := seedAPIToken(t, st, ScopeInstanceAdmin, nil, false, false)
	adminJWT := jwtFor(t, st, jwtSvc, "admin@x", store.RoleAdmin)

	// Gate constructed with API tokens DISABLED.
	gate := RequireAdminOrToken(jwtSvc, st, false)(okHandler())

	if code, body := statusFor(gate, adminTok); code != http.StatusUnauthorized {
		t.Fatalf("valid instance-admin token with tokens disabled: status=%d want 401; body=%s", code, body)
	}
	// The JWT path must still work — disabling tokens must not break admins.
	if code, body := statusFor(gate, adminJWT); code != http.StatusOK {
		t.Fatalf("admin JWT with tokens disabled: status=%d want 200; body=%s", code, body)
	}
}

func bodyHasCode(body, code string) bool {
	return len(body) > 0 && (contains(body, `"code":"`+code+`"`) || contains(body, `"code": "`+code+`"`))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
