package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	intauth "github.com/clawvisor/clawvisor/internal/auth"
	pkgauth "github.com/clawvisor/clawvisor/pkg/auth"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func flatTeamStore(t *testing.T) store.Store {
	t.Helper()
	db, err := sqlite.New(context.Background(), filepath.Join(t.TempDir(), "flatteam.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.NewStore(db)
}

func flatTeamAuth(t *testing.T, st store.Store, cfg config.AuthConfig) *AuthHandler {
	t.Helper()
	if cfg.AccessTokenTTL == "" {
		cfg.AccessTokenTTL = "15m"
	}
	if cfg.RefreshTokenTTL == "" {
		cfg.RefreshTokenTTL = "720h"
	}
	jwtSvc, err := intauth.NewJWTService("test-secret-test-secret-test-secret-12345")
	if err != nil {
		t.Fatalf("NewJWTService: %v", err)
	}
	return NewAuthHandler(jwtSvc, st, cfg, pkgauth.MagicTokenStore(nil), "https://cv.example", true)
}

func registerJSON(t *testing.T, h *AuthHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Register(rec, req)
	return rec
}

func adminCtx(u *store.User) context.Context {
	return context.WithValue(context.Background(), middleware.UserContextKey, u)
}

func bodyHasCode(body, code string) bool {
	return strings.Contains(body, `"code":"`+code+`"`) || strings.Contains(body, `"code": "`+code+`"`)
}

func TestFirstUserBecomesAdmin(t *testing.T) {
	st := flatTeamStore(t)
	h := flatTeamAuth(t, st, config.AuthConfig{})

	if rec := registerJSON(t, h, `{"email":"founder@x","password":"hunter2hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first register: %d %s", rec.Code, rec.Body.String())
	}
	if rec := registerJSON(t, h, `{"email":"second@x","password":"hunter2hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("second register: %d %s", rec.Code, rec.Body.String())
	}
	founder, _ := st.GetUserByEmail(context.Background(), "founder@x")
	second, _ := st.GetUserByEmail(context.Background(), "second@x")
	if founder.Role != store.RoleAdmin {
		t.Fatalf("founder role=%q want admin", founder.Role)
	}
	if second.Role != store.RoleMember {
		t.Fatalf("second role=%q want member", second.Role)
	}
}

func TestInviteFlow(t *testing.T) {
	st := flatTeamStore(t)
	ctx := context.Background()
	admin, err := st.CreateUser(ctx, "admin@x", "hash", store.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	users := NewUsersHandler(st, "https://cv.example", nil)
	auth := flatTeamAuth(t, st, config.AuthConfig{})

	// Mint a member invite pinned to an email.
	mintReq := httptest.NewRequest(http.MethodPost, "/api/users/invites",
		strings.NewReader(`{"email":"invitee@x","role":"member"}`)).WithContext(adminCtx(admin))
	mintRec := httptest.NewRecorder()
	users.CreateInvite(mintRec, mintReq)
	if mintRec.Code != http.StatusCreated {
		t.Fatalf("mint invite: %d %s", mintRec.Code, mintRec.Body.String())
	}
	var mint struct {
		InviteToken string `json:"invite_token"`
		InviteURL   string `json:"invite_url"`
	}
	json.Unmarshal(mintRec.Body.Bytes(), &mint)
	if !strings.HasPrefix(mint.InviteToken, "cvinv_") {
		t.Fatalf("invite token shape: %q", mint.InviteToken)
	}
	if !strings.Contains(mint.InviteURL, "/join?token=cvinv_") {
		t.Fatalf("invite_url: %q", mint.InviteURL)
	}

	// Claim: role honored (member), account pending_verification, no tokens.
	claim := registerJSON(t, auth, `{"email":"invitee@x","password":"hunter2hunter2","invite_token":"`+mint.InviteToken+`"}`)
	if claim.Code != http.StatusCreated {
		t.Fatalf("claim: %d %s", claim.Code, claim.Body.String())
	}
	if !strings.Contains(claim.Body.String(), "pending_verification") {
		t.Fatalf("claim body = %s, want pending_verification", claim.Body.String())
	}
	invitee, _ := st.GetUserByEmail(ctx, "invitee@x")
	if invitee.Role != store.RoleMember {
		t.Fatalf("invitee role=%q want member", invitee.Role)
	}
	if invitee.Verified() {
		t.Fatal("invitee must start pending_verification")
	}

	// Single-use: re-claim rejected.
	if again := registerJSON(t, auth, `{"email":"invitee@x","password":"hunter2hunter2","invite_token":"`+mint.InviteToken+`"}`); again.Code != http.StatusConflict {
		t.Fatalf("re-claim: %d want 409", again.Code)
	}

	// Expired invite rejected.
	expired := &store.UserInvite{
		TokenHash: intauth.HashToken("cvinv_deadbeef"),
		Role:      store.RoleMember,
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	if err := st.CreateUserInvite(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if rec := registerJSON(t, auth, `{"email":"late@x","password":"hunter2hunter2","invite_token":"cvinv_deadbeef"}`); rec.Code != http.StatusForbidden || !bodyHasCode(rec.Body.String(), "INVITE_EXPIRED") {
		t.Fatalf("expired claim: %d %s", rec.Code, rec.Body.String())
	}
}

func TestInviteFlow_RequireInvite(t *testing.T) {
	// require_invite blocks tokenless registration, except the first user.
	st := flatTeamStore(t)
	h := flatTeamAuth(t, st, config.AuthConfig{RequireInvite: true})

	// First user always allowed and becomes admin.
	first := registerJSON(t, h, `{"email":"founder@x","password":"hunter2hunter2"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first user under require_invite: %d %s", first.Code, first.Body.String())
	}
	// Second tokenless registration blocked.
	second := registerJSON(t, h, `{"email":"nope@x","password":"hunter2hunter2"}`)
	if second.Code != http.StatusForbidden || !bodyHasCode(second.Body.String(), "INVITE_REQUIRED") {
		t.Fatalf("tokenless under require_invite: %d %s", second.Code, second.Body.String())
	}
}

func TestUserCRUD_LastAdminGuard(t *testing.T) {
	st := flatTeamStore(t)
	ctx := context.Background()
	admin, _ := st.CreateUser(ctx, "admin@x", "hash", store.RoleAdmin)
	users := NewUsersHandler(st, "", nil)

	// Cannot demote the only admin.
	demote := func(id, role string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/users/"+id+"/role",
			strings.NewReader(`{"role":"`+role+`"}`)).WithContext(adminCtx(admin))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		users.UpdateRole(rec, req)
		return rec
	}
	if rec := demote(admin.ID, "member"); rec.Code != http.StatusConflict || !bodyHasCode(rec.Body.String(), "LAST_ADMIN") {
		t.Fatalf("demote only admin: %d %s", rec.Code, rec.Body.String())
	}

	// Cannot delete the only admin.
	del := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/api/users/"+id, nil).WithContext(adminCtx(admin))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		users.DeleteUser(rec, req)
		return rec
	}
	if rec := del(admin.ID); rec.Code != http.StatusConflict || !bodyHasCode(rec.Body.String(), "LAST_ADMIN") {
		t.Fatalf("delete only admin: %d %s", rec.Code, rec.Body.String())
	}

	// With a second admin, both operations succeed.
	admin2, _ := st.CreateUser(ctx, "admin2@x", "hash", store.RoleAdmin)
	if rec := demote(admin2.ID, "member"); rec.Code != http.StatusOK {
		t.Fatalf("demote with 2 admins: %d %s", rec.Code, rec.Body.String())
	}
	admin3, _ := st.CreateUser(ctx, "admin3@x", "hash", store.RoleAdmin)
	_ = admin3
	if rec := del(admin.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete with another admin: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMaxUsersExcludesInstanceUser(t *testing.T) {
	st := flatTeamStore(t)
	h := flatTeamAuth(t, st, config.AuthConfig{MaxUsers: 2})

	// Two real users fit despite the seeded _instance row.
	if rec := registerJSON(t, h, `{"email":"a@x","password":"hunter2hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("user 1: %d %s", rec.Code, rec.Body.String())
	}
	if rec := registerJSON(t, h, `{"email":"b@x","password":"hunter2hunter2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("user 2: %d %s", rec.Code, rec.Body.String())
	}
	// Third exceeds the cap.
	if rec := registerJSON(t, h, `{"email":"c@x","password":"hunter2hunter2"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("user 3: %d want 403 %s", rec.Code, rec.Body.String())
	}
}

func TestInstanceUserCannotLogin(t *testing.T) {
	st := flatTeamStore(t)
	h := flatTeamAuth(t, st, config.AuthConfig{})

	// The seeded _instance row has a non-bcrypt sentinel hash → login fails.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"email":"instance@system.clawvisor.invalid","password":"!locked!"}`))
	rec := httptest.NewRecorder()
	h.Login(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("instance login: %d want 401 %s", rec.Code, rec.Body.String())
	}

	// And it cannot be deleted through the API.
	users := NewUsersHandler(st, "", nil)
	dreq := httptest.NewRequest(http.MethodDelete, "/api/users/_instance", nil)
	dreq.SetPathValue("id", store.InstanceUserID)
	drec := httptest.NewRecorder()
	users.DeleteUser(drec, dreq)
	if drec.Code != http.StatusBadRequest {
		t.Fatalf("delete _instance: %d want 400 %s", drec.Code, drec.Body.String())
	}
}

// TestOffboardingRevokesAgentToken pins the offboarding invariant: deleting
// a user immediately invalidates their agent (`cvis_`) tokens server-side —
// the token fails on its next lookup without touching the laptop.
func TestOffboardingRevokesAgentToken(t *testing.T) {
	st := flatTeamStore(t)
	ctx := context.Background()
	// An admin must remain so deleting the member isn't a last-admin case.
	if _, err := st.CreateUser(ctx, "admin@x", "hash", store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser(ctx, "member@x", "hash", store.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	rawToken, err := intauth.GenerateAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	hash := intauth.HashToken(rawToken)
	if _, err := st.CreateAgent(ctx, member.ID, "laptop", hash); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAgentByToken(ctx, hash); err != nil {
		t.Fatalf("agent token should resolve before delete: %v", err)
	}

	users := NewUsersHandler(st, "", nil)
	dreq := httptest.NewRequest(http.MethodDelete, "/api/users/"+member.ID, nil)
	dreq.SetPathValue("id", member.ID)
	drec := httptest.NewRecorder()
	users.DeleteUser(drec, dreq)
	if drec.Code != http.StatusNoContent {
		t.Fatalf("delete member: %d %s", drec.Code, drec.Body.String())
	}

	// Next-request enforcement: the token is now inert (cascade removed the
	// agent row), so GetAgentByToken — the auth path's lookup — rejects it.
	if _, err := st.GetAgentByToken(ctx, hash); err == nil {
		t.Fatal("deleted user's agent token still resolves — offboarding invariant broken")
	}
}
