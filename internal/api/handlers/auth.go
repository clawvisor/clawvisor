package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ericlevine/clawvisor/internal/auth"
	"github.com/ericlevine/clawvisor/internal/api/middleware"
	"github.com/ericlevine/clawvisor/internal/config"
	"github.com/ericlevine/clawvisor/internal/store"
)

// AuthHandler handles user registration, login, token refresh, and logout.
type AuthHandler struct {
	jwtSvc *auth.JWTService
	st     store.Store
	cfg    config.AuthConfig
}

func NewAuthHandler(jwtSvc *auth.JWTService, st store.Store, cfg config.AuthConfig) *AuthHandler {
	return &AuthHandler{jwtSvc: jwtSvc, st: st, cfg: cfg}
}

type authResponse struct {
	User         *store.User `json:"user"`
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
}

// Register creates a new user account.
//
// POST /api/auth/register
// Body: {"email": "...", "password": "..."}
func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body")
		return
	}
	if body.Email == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "email and password are required")
		return
	}

	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD", err.Error())
		return
	}

	user, err := h.st.CreateUser(r.Context(), body.Email, hash)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "EMAIL_TAKEN", "an account with that email already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create user")
		return
	}

	resp, err := h.issueTokens(r, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not issue tokens")
		return
	}
	writeJSON(w, http.StatusCreated, resp)
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
		// Return generic message to avoid user enumeration
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid email or password")
		return
	}

	if err := auth.CheckPassword(body.Password, user.PasswordHash); err != nil {
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid email or password")
		return
	}

	resp, err := h.issueTokens(r, user)
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "refresh_token is required")
		return
	}

	tokenHash := hashToken(body.RefreshToken)
	sess, err := h.st.GetSession(r.Context(), tokenHash)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "invalid or expired refresh token")
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		_ = h.st.DeleteSession(r.Context(), tokenHash)
		writeError(w, http.StatusUnauthorized, "TOKEN_EXPIRED", "refresh token has expired")
		return
	}

	user, err := h.st.GetUserByID(r.Context(), sess.UserID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "USER_NOT_FOUND", "user no longer exists")
		return
	}

	// Invalidate old session before issuing new one (rotation)
	_ = h.st.DeleteSession(r.Context(), tokenHash)

	resp, err := h.issueTokens(r, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not issue tokens")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Logout invalidates the current session's refresh token.
//
// POST /api/auth/logout
// Auth: Bearer <access_token>
// Body: {"refresh_token": "..."}  (optional; clears the specific session)
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if body.RefreshToken != "" {
		_ = h.st.DeleteSession(r.Context(), hashToken(body.RefreshToken))
	}

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

// ── Helpers ───────────────────────────────────────────────────────────────────

func (h *AuthHandler) issueTokens(r *http.Request, user *store.User) (*authResponse, error) {
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

	rawRefresh, err := generateToken()
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().Add(refreshTTL)
	if _, err := h.st.CreateSession(r.Context(), user.ID, hashToken(rawRefresh), expiresAt); err != nil {
		return nil, err
	}

	return &authResponse{
		User:         user,
		AccessToken:  accessToken,
		RefreshToken: rawRefresh,
	}, nil
}

// generateToken creates a cryptographically secure random token string.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashToken returns the SHA-256 hex digest of a token.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
