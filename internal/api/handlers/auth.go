package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	pkgauth "github.com/clawvisor/clawvisor/pkg/auth"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// AuthHandler handles user registration, login, token refresh, and logout.
type AuthHandler struct {
	jwtSvc     pkgauth.TokenService
	st         store.Store
	cfg        config.AuthConfig
	magicStore pkgauth.MagicTokenStore // nil when magic link auth is disabled
	baseURL    string
	isLocal    bool // true when server is bound to loopback/wildcard (allows Docker bridge IPs for magic local auth)
}

func NewAuthHandler(jwtSvc pkgauth.TokenService, st store.Store, cfg config.AuthConfig, magicStore pkgauth.MagicTokenStore, baseURL string, isLocal bool) *AuthHandler {
	return &AuthHandler{jwtSvc: jwtSvc, st: st, cfg: cfg, magicStore: magicStore, baseURL: baseURL, isLocal: isLocal}
}

type authResponse struct {
	User         *store.User `json:"user"`
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token,omitempty"`
}

const refreshTokenCookieName = "clawvisor_refresh_token"

// Register creates a new user account.
//
// POST /api/auth/register
// Body: {"email": "...", "password": "...", "invite_token"?: "cvinv_..."}
//
// Roles & invites (spec 04):
//   - The very first user (CountUsers == 0) always becomes admin; every
//     subsequent user is a member.
//   - A valid invite_token bypasses AllowedEmails (still respects MaxUsers)
//     and, when auth.require_invite is set, is what makes registration
//     possible at all. An invite claimed over this enrollment channel can
//     only ever produce a member (invite security rule 1: the token rides
//     through argv/env, so it must not grant admin — promotion is a
//     deliberate admin act via PUT /api/users/{id}/role).
//   - An invite-claimed account is created pending_verification and cannot
//     authenticate or mint an agent token until the invitee proves email
//     possession via the magic-link confirm (rule 2). No auth tokens are
//     issued on claim.
func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		InviteToken string `json:"invite_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body")
		return
	}
	if body.Email == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "email and password are required")
		return
	}

	count, err := h.st.CountUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not check user count")
		return
	}
	isFirstUser := count == 0

	// Resolve the invite (if one was supplied) before any gate: it both
	// unlocks require_invite installs and bypasses AllowedEmails.
	var invite *store.UserInvite
	if strings.TrimSpace(body.InviteToken) != "" {
		invite, err = h.resolveInvite(r, body.InviteToken, body.Email)
		if err != nil {
			writeInviteError(w, err)
			return
		}
	}

	if h.cfg.RequireInvite && invite == nil && !isFirstUser {
		writeError(w, http.StatusForbidden, "INVITE_REQUIRED", "registration requires a valid invite")
		return
	}

	if h.cfg.MaxUsers > 0 && count >= h.cfg.MaxUsers {
		writeError(w, http.StatusForbidden, "REGISTRATION_DISABLED", "maximum number of users reached")
		return
	}

	// AllowedEmails is bypassed by a valid invite; otherwise enforced.
	if invite == nil && len(h.cfg.AllowedEmails) > 0 {
		allowed := false
		for _, e := range h.cfg.AllowedEmails {
			if strings.EqualFold(e, body.Email) {
				allowed = true
				break
			}
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "REGISTRATION_DISABLED", "registration is not open")
			return
		}
	}

	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD", err.Error())
		return
	}

	// Invite-claim path: create a pending_verification member and burn the
	// invite. No tokens are issued — the invitee confirms email possession
	// via the magic-link flow first.
	if invite != nil {
		user, err := h.st.CreateInvitedUser(r.Context(), body.Email, hash, store.RoleMember)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeError(w, http.StatusConflict, "EMAIL_TAKEN", "an account with that email already exists")
				return
			}
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create user")
			return
		}
		if err := h.st.MarkUserInviteUsed(r.Context(), invite.ID, user.ID); err != nil {
			// Lost a single-use race: another claim already burned it.
			_ = h.st.DeleteUser(r.Context(), user.ID)
			writeError(w, http.StatusConflict, "INVITE_ALREADY_USED", "invite has already been claimed")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"status":  "pending_verification",
			"user_id": user.ID,
			"email":   user.Email,
		})
		return
	}

	// Non-invite path: first user is admin, everyone else a member; the
	// account is verified immediately and receives a token pair (unchanged
	// open-registration behavior).
	role := store.RoleMember
	if isFirstUser {
		role = store.RoleAdmin
	}
	user, err := h.st.CreateUser(r.Context(), body.Email, hash, role)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "EMAIL_TAKEN", "an account with that email already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create user")
		return
	}

	resp, err := h.issueTokens(w, r, user)
	if err != nil {
		_ = h.st.DeleteUser(r.Context(), user.ID)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not issue tokens")
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// errInvite* are resolveInvite failure sentinels mapped to HTTP by
// writeInviteError so the plaintext token is never echoed.
var (
	errInviteNotFound = errors.New("invite not found")
	errInviteExpired  = errors.New("invite expired")
	errInviteUsed     = errors.New("invite already used")
	errInviteEmail    = errors.New("invite email mismatch")
)

// resolveInvite validates a plaintext cvinv_ token against the store: it
// must exist, be unused, be unexpired, and (when the invite pinned an
// email) match the registrant's email case-insensitively.
func (h *AuthHandler) resolveInvite(r *http.Request, token, email string) (*store.UserInvite, error) {
	inv, err := h.st.GetUserInviteByHash(r.Context(), auth.HashToken(strings.TrimSpace(token)))
	if err != nil {
		return nil, errInviteNotFound
	}
	if inv.UsedAt != nil {
		return nil, errInviteUsed
	}
	if time.Now().After(inv.ExpiresAt) {
		return nil, errInviteExpired
	}
	if inv.Email != "" && !strings.EqualFold(inv.Email, email) {
		return nil, errInviteEmail
	}
	return inv, nil
}

func writeInviteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInviteExpired):
		writeError(w, http.StatusForbidden, "INVITE_EXPIRED", "invite has expired")
	case errors.Is(err, errInviteUsed):
		writeError(w, http.StatusConflict, "INVITE_ALREADY_USED", "invite has already been claimed")
	case errors.Is(err, errInviteEmail):
		writeError(w, http.StatusForbidden, "INVITE_EMAIL_MISMATCH", "invite is bound to a different email")
	default:
		writeError(w, http.StatusForbidden, "INVITE_INVALID", "invite is not valid")
	}
}

// Login authenticates a user and returns a token pair.
//
// POST /api/auth/login
// Body: {"email": "...", "password": "..."}
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body")
		return
	}

	user, err := h.st.GetUserByEmail(r.Context(), body.Email)
	if err != nil {
		// Spend a bcrypt comparison on the not-found path so the
		// response timing doesn't distinguish "no such email" from
		// "wrong password". Without this, a network observer can
		// enumerate registered accounts by the latency delta between
		// an immediate 401 and a cost-12 bcrypt 401.
		auth.DummyCheckPassword(body.Password)
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid email or password")
		return
	}

	if err := auth.CheckPassword(body.Password, user.PasswordHash); err != nil {
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid email or password")
		return
	}

	// Email-possession proof (invite security rule 2): a pending_verification
	// account cannot authenticate until it confirms the email via magic link.
	if !user.Verified() {
		writeError(w, http.StatusForbidden, "EMAIL_NOT_VERIFIED", "confirm your email via the magic link before signing in")
		return
	}

	resp, err := h.issueTokens(w, r, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not issue tokens")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Refresh rotates a refresh token and issues a new token pair.
//
// POST /api/auth/refresh
// Body: {"refresh_token": "..."}
func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "refresh_token is required")
		return
	}
	refreshToken := body.RefreshToken
	if refreshToken == "" {
		cookie, err := r.Cookie(refreshTokenCookieName)
		if err == nil {
			refreshToken = cookie.Value
		}
	}
	if refreshToken == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "refresh_token is required")
		return
	}

	tokenHash := auth.HashToken(refreshToken)
	// Atomic delete-and-return so a stolen refresh token replayed
	// concurrently produces at most one new token pair: the second caller
	// gets ErrNotFound because the first deleted the row first.
	sess, err := h.st.ConsumeSession(r.Context(), tokenHash)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "invalid or expired refresh token")
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		writeError(w, http.StatusUnauthorized, "TOKEN_EXPIRED", "refresh token has expired")
		return
	}

	user, err := h.st.GetUserByID(r.Context(), sess.UserID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "USER_NOT_FOUND", "user no longer exists")
		return
	}

	resp, err := h.issueTokens(w, r, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not issue tokens")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Logout invalidates the current session's refresh token.
//
// POST /api/auth/logout
// Auth: optional Bearer <access_token>
// Body: {"refresh_token": "..."}  (optional; clears the specific session)
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if body.RefreshToken != "" {
		_ = h.st.DeleteSession(r.Context(), auth.HashToken(body.RefreshToken))
	} else if cookie, err := r.Cookie(refreshTokenCookieName); err == nil && cookie.Value != "" {
		_ = h.st.DeleteSession(r.Context(), auth.HashToken(cookie.Value))
	} else if user != nil {
		// No specific token provided — clear all sessions for the user.
		_ = h.st.DeleteUserSessions(r.Context(), user.ID)
	}
	h.clearRefreshCookie(w, r)

	w.WriteHeader(http.StatusNoContent)
}

// Me returns the currently authenticated user.
//
// GET /api/me
// Auth: Bearer <access_token>
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// UpdateMe updates the current user's password.
//
// PUT /api/me
// Auth: Bearer <access_token>
// Body: {"current_password": "...", "new_password": "..."}
func (h *AuthHandler) UpdateMe(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.CurrentPassword == "" || body.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "current_password and new_password are required")
		return
	}

	// Re-fetch the full user record so we have the password hash.
	full, err := h.st.GetUserByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load user")
		return
	}
	if err := auth.CheckPassword(body.CurrentPassword, full.PasswordHash); err != nil {
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "current password is incorrect")
		return
	}

	newHash, err := auth.HashPassword(body.NewPassword)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD", err.Error())
		return
	}
	if err := h.st.UpdateUserPassword(r.Context(), user.ID, newHash); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update password")
		return
	}

	// Revoke all existing sessions so stolen refresh tokens become invalid.
	_ = h.st.DeleteUserSessions(r.Context(), user.ID)

	updated, err := h.st.GetUserByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not reload user")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// DeleteMe permanently deletes the current user account and all associated data.
//
// DELETE /api/me
// Auth: Bearer <access_token>
// Body: {"password": "..."}  (required confirmation)
func (h *AuthHandler) DeleteMe(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Password == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "password is required to confirm deletion")
		return
	}

	full, err := h.st.GetUserByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load user")
		return
	}
	if err := auth.CheckPassword(body.Password, full.PasswordHash); err != nil {
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "password is incorrect")
		return
	}

	if err := h.st.DeleteUser(r.Context(), user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete account")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ExchangeMagic validates a magic token via JSON and returns a token pair.
// Used by the SPA's MagicLink page and by the TUI.
//
// POST /api/auth/magic
// Body: {"token": "..."}
func (h *AuthHandler) ExchangeMagic(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "token is required")
		return
	}
	if h.magicStore == nil {
		writeError(w, http.StatusBadRequest, "NOT_ENABLED", "magic link auth is not enabled")
		return
	}

	userID, err := h.magicStore.Validate(body.Token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "token expired or already used")
		return
	}

	user, err := h.st.GetUserByID(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "USER_NOT_FOUND", "user not found")
		return
	}

	// Exchanging a magic link proves control of the account's email — this
	// is the email-possession confirm that flips a pending_verification
	// invite-claimed account to usable (invite security rule 2). Idempotent.
	if !user.Verified() {
		if err := h.st.MarkUserVerified(r.Context(), user.ID); err == nil {
			now := time.Now().UTC()
			user.VerifiedAt = &now
		}
	}

	resp, err := h.issueTokens(w, r, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not issue tokens")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// GenerateMagicLocal generates a fresh magic token for the admin@local user.
// This endpoint is restricted to localhost connections and requires no auth,
// so the CLI can always get a valid token to open the dashboard.
//
// POST /api/auth/magic/local
func (h *AuthHandler) GenerateMagicLocal(w http.ResponseWriter, r *http.Request) {
	if h.magicStore == nil {
		writeError(w, http.StatusBadRequest, "NOT_ENABLED", "magic link auth is not enabled")
		return
	}

	// Only allow requests from localhost. When the server is in local mode
	// (bound to loopback or 0.0.0.0), skip the check — this covers Docker
	// port-mapping where the remote address is the bridge IP, not 127.0.0.1.
	if !h.isLocal {
		host := r.RemoteAddr
		// RemoteAddr is "host:port"; strip the port.
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			host = host[:idx]
		}
		// Strip brackets from IPv6 addresses (e.g. "[::1]" → "::1").
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
		if host != "127.0.0.1" && host != "::1" && host != "localhost" {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "this endpoint is only available from localhost")
			return
		}
	}

	const localEmail = "admin@local"
	user, err := h.st.GetUserByEmail(r.Context(), localEmail)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "local user not found")
		return
	}

	token, err := h.magicStore.Generate(user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate token")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (h *AuthHandler) issueTokens(w http.ResponseWriter, r *http.Request, user *store.User) (*authResponse, error) {
	accessTTL, err := h.cfg.AccessTokenDuration()
	if err != nil {
		return nil, err
	}
	refreshTTL, err := h.cfg.RefreshTokenDuration()
	if err != nil {
		return nil, err
	}

	accessToken, err := h.jwtSvc.GenerateAccessToken(user.ID, user.Email, accessTTL)
	if err != nil {
		return nil, err
	}

	rawRefresh, err := auth.GenerateRandomToken()
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().Add(refreshTTL)
	if _, err := h.st.CreateSession(r.Context(), user.ID, auth.HashToken(rawRefresh), expiresAt); err != nil {
		return nil, err
	}
	h.setRefreshCookie(w, r, rawRefresh, expiresAt)

	resp := &authResponse{
		User:         user,
		AccessToken:  accessToken,
		RefreshToken: rawRefresh,
	}
	if isBrowserRequest(r) {
		resp.RefreshToken = ""
	}
	return resp, nil
}

func (h *AuthHandler) setRefreshCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshTokenCookieName,
		Value:    token,
		Path:     "/api/auth",
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
		HttpOnly: true,
		Secure:   h.secureRefreshCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *AuthHandler) clearRefreshCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshTokenCookieName,
		Value:    "",
		Path:     "/api/auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secureRefreshCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *AuthHandler) secureRefreshCookie(r *http.Request) bool {
	return r.TLS != nil || !h.isLocal
}

func isBrowserRequest(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") != "" ||
		r.Header.Get("Sec-Fetch-Mode") != "" ||
		r.Header.Get("Origin") != "" ||
		r.Header.Get("Referer") != ""
}
