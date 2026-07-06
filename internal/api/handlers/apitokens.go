package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// APITokensHandler manages the lifecycle of long-lived, scoped, revocable
// API tokens (spec 05). All three routes are admin-gated: a JWT admin (via
// spec 04's role system, once it lands) or an `instance-admin` API token.
// In 05-lite the only issuable scope is `instance-admin`.
type APITokensHandler struct {
	st     store.Store
	logger *slog.Logger
}

func NewAPITokensHandler(st store.Store, logger *slog.Logger) *APITokensHandler {
	return &APITokensHandler{st: st, logger: logger}
}

// Create mints a new API token and returns the plaintext exactly once.
//
// POST /api/tokens
// Auth: admin (JWT admin or instance-admin API token)
// Body: {"name":"terraform","scope":"instance-admin","expires_at"?:RFC3339}
//
// Burn-on-first-use: when the authenticating credential is the bootstrap
// token, a successful mint revokes the bootstrap token immediately so it
// can never become a permanent instance-admin credential.
func (h *APITokensHandler) Create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		Scope     string `json:"scope"`
		ExpiresAt string `json:"expires_at"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name is required")
		return
	}

	// 05-lite issues a single scope. Empty defaults to instance-admin; any
	// other value (config-write / config-read) is rejected until spec 04
	// lands the scope split.
	scope := body.Scope
	if scope == "" {
		scope = middleware.ScopeInstanceAdmin
	}
	if scope != middleware.ScopeInstanceAdmin {
		writeError(w, http.StatusBadRequest, "INVALID_SCOPE",
			"only the \"instance-admin\" scope is supported in this build; config-write/config-read land with roles")
		return
	}

	var expiresAt *time.Time
	if body.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "expires_at must be RFC3339")
			return
		}
		expiresAt = &t
	}

	raw, prefix, err := auth.GenerateAPIToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate token")
		return
	}

	var createdBy *string
	if u := middleware.UserFromContext(r.Context()); u != nil && u.ID != store.InstanceUserID {
		id := u.ID
		createdBy = &id
	}

	tok := &store.APIToken{
		Name:        body.Name,
		TokenHash:   auth.HashToken(raw),
		TokenPrefix: prefix,
		Scope:       scope,
		CreatedBy:   createdBy,
		ExpiresAt:   expiresAt,
	}
	if err := h.st.CreateAPIToken(r.Context(), tok); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create token")
		return
	}

	// Burn-on-first-use: only a successful scoped-token mint burns the
	// bootstrap credential. Reads/other calls never reach here, so a failed
	// apply can retry the mint until it succeeds.
	if bt := middleware.APITokenFromContext(r.Context()); bt != nil && bt.IsBootstrap {
		if err := h.st.RevokeAPIToken(r.Context(), bt.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			// The token IS minted; log but don't fail the mint. The bootstrap
			// token still hard-expires at +24h even if this best-effort
			// revoke didn't land.
			h.logger.Error("failed to burn bootstrap token after mint", "err", err, "bootstrap_token_id", bt.ID)
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           tok.ID,
		"token":        raw,
		"token_prefix": tok.TokenPrefix,
		"scope":        tok.Scope,
	})
}

// List returns all API tokens (never hashes or plaintext).
//
// GET /api/tokens
// Auth: admin (JWT admin or instance-admin API token)
func (h *APITokensHandler) List(w http.ResponseWriter, r *http.Request) {
	tokens, err := h.st.ListAPITokens(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list tokens")
		return
	}
	out := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, map[string]any{
			"id":           t.ID,
			"name":         t.Name,
			"token_prefix": t.TokenPrefix,
			"scope":        t.Scope,
			"created_by":   t.CreatedBy,
			"created_at":   t.CreatedAt,
			"expires_at":   t.ExpiresAt,
			"last_used_at": t.LastUsedAt,
			"revoked_at":   t.RevokedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// Delete soft-revokes a token (sets revoked_at; row kept for audit).
//
// DELETE /api/tokens/{id}
// Auth: admin (JWT admin or instance-admin API token)
func (h *APITokensHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "token id is required")
		return
	}
	if err := h.st.RevokeAPIToken(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "token not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not revoke token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
