package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// inviteTTL is the default lifetime of an invite. Shortened to 48h from a
// week: an invite URL is a bearer secret, so it should expire quickly
// (invite security rule 5).
const inviteTTL = 48 * time.Hour

// UsersHandler serves the admin user-management surface (spec 04): invites
// plus user list / role-change / delete. Every route is admin-gated in
// server.go via RequireAdminOrToken — a JWT admin OR an instance-admin API
// token (so the Terraform employee-onboarding flow can run headlessly). A
// config-write token is 403 INSUFFICIENT_SCOPE before reaching these
// handlers; a member JWT is 403 FORBIDDEN.
type UsersHandler struct {
	st      store.Store
	baseURL string
	logger  *slog.Logger
}

func NewUsersHandler(st store.Store, baseURL string, logger *slog.Logger) *UsersHandler {
	return &UsersHandler{st: st, baseURL: strings.TrimRight(baseURL, "/"), logger: logger}
}

// CreateInvite mints a single-use, 48h invite and returns the plaintext
// token + URL exactly once.
//
// POST /api/users/invites
// Body: {"email"?: "...", "role"?: "member"|"admin"}
func (h *UsersHandler) CreateInvite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	email := strings.TrimSpace(body.Email)
	role := body.Role
	if role == "" {
		role = store.RoleMember
	}
	if role != store.RoleMember && role != store.RoleAdmin {
		writeError(w, http.StatusBadRequest, "INVALID_ROLE", "role must be \"member\" or \"admin\"")
		return
	}
	// Rule 4: an any-email (unpinned) invite may only be member — an
	// unpinned admin invite is a fleet-wide privilege-escalation bearer
	// secret. Reject it at mint time.
	if email == "" && role == store.RoleAdmin {
		writeError(w, http.StatusBadRequest, "INVALID_INVITE",
			"an any-email invite may only grant the member role")
		return
	}

	raw, err := auth.GenerateInviteToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate invite")
		return
	}
	expiresAt := time.Now().Add(inviteTTL).UTC()

	inv := &store.UserInvite{
		TokenHash: auth.HashToken(raw),
		Email:     email,
		Role:      role,
		CreatedBy: h.actingUserID(r),
		ExpiresAt: expiresAt,
	}
	if err := h.st.CreateUserInvite(r.Context(), inv); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create invite")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           inv.ID,
		"invite_token": raw,
		"invite_url":   h.inviteURL(raw),
		"email":        inv.Email,
		"role":         inv.Role,
		"expires_at":   expiresAt,
	})
}

// ListInvites lists pending (unclaimed) invites. Never returns hashes.
//
// GET /api/users/invites
func (h *UsersHandler) ListInvites(w http.ResponseWriter, r *http.Request) {
	invites, err := h.st.ListPendingUserInvites(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list invites")
		return
	}
	out := make([]map[string]any, 0, len(invites))
	for _, inv := range invites {
		out = append(out, map[string]any{
			"id":         inv.ID,
			"email":      inv.Email,
			"role":       inv.Role,
			"expires_at": inv.ExpiresAt,
			"created_by": inv.CreatedBy,
			"created_at": inv.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
}

// DeleteInvite revokes a pending invite.
//
// DELETE /api/users/invites/{id}
func (h *UsersHandler) DeleteInvite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invite id is required")
		return
	}
	if err := h.st.DeleteUserInvite(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "invite not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete invite")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListUsers lists real users (system rows excluded by the store).
//
// GET /api/users
func (h *UsersHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.st.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list users")
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"id":         u.ID,
			"email":      u.Email,
			"role":       u.Role,
			"verified":   u.Verified(),
			"created_at": u.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// UpdateRole changes a user's role, guarding the last admin.
//
// PUT /api/users/{id}/role
// Body: {"role":"admin"|"member"}
func (h *UsersHandler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || id == store.InstanceUserID {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid user id")
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Role != store.RoleAdmin && body.Role != store.RoleMember {
		writeError(w, http.StatusBadRequest, "INVALID_ROLE", "role must be \"admin\" or \"member\"")
		return
	}

	target, err := h.st.GetUserByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}
	// Demoting the only admin would lock the instance out of its admin
	// surface — refuse it (Q: last-admin guard).
	if target.Role == store.RoleAdmin && body.Role == store.RoleMember {
		if h.onlyAdmin(w, r) {
			return
		}
	}
	if err := h.st.UpdateUserRole(r.Context(), id, body.Role); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update role")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "role": body.Role})
}

// DeleteUser deletes a user. Deleting the row cascades the user's agents,
// so their `cvis_` tokens go inert on the next request (offboarding
// invariant); audit/cost rows survive (user_id → NULL, actor_email
// retained). Guards: `_instance` is undeletable; the only admin is
// protected; deleting yourself is allowed only when another admin remains.
//
// DELETE /api/users/{id}
func (h *UsersHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "user id is required")
		return
	}
	if id == store.InstanceUserID {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "the instance system user cannot be deleted")
		return
	}

	target, err := h.st.GetUserByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}
	// The only admin is protected whether they delete themselves or another
	// admin deletes them — this also enforces "self-delete only when another
	// admin exists".
	if target.Role == store.RoleAdmin {
		if h.onlyAdmin(w, r) {
			return
		}
	}
	if err := h.st.DeleteUser(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// onlyAdmin writes a 409 LAST_ADMIN and returns true when exactly one admin
// remains (so the caller should abort).
func (h *UsersHandler) onlyAdmin(w http.ResponseWriter, r *http.Request) bool {
	n, err := h.st.CountAdmins(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not count admins")
		return true
	}
	if n <= 1 {
		writeError(w, http.StatusConflict, "LAST_ADMIN", "cannot remove the last admin")
		return true
	}
	return false
}

// actingUserID returns the JWT admin's id for created_by attribution, or nil
// for token-authenticated calls (the acting principal is the token; the
// injected `_instance` user is not a meaningful creator).
func (h *UsersHandler) actingUserID(r *http.Request) *string {
	if middleware.APITokenFromContext(r.Context()) != nil {
		return nil
	}
	if u := middleware.UserFromContext(r.Context()); u != nil && u.ID != store.InstanceUserID {
		id := u.ID
		return &id
	}
	return nil
}

func (h *UsersHandler) inviteURL(rawToken string) string {
	base := h.baseURL
	if base == "" {
		return "/join?token=" + rawToken
	}
	return base + "/join?token=" + rawToken
}
