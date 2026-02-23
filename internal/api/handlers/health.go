package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/ericlevine/clawvisor/internal/store"
	"github.com/ericlevine/clawvisor/internal/vault"
)

// HealthHandler handles /health and /ready.
type HealthHandler struct {
	st store.Store
	v  vault.Vault
}

func NewHealthHandler(st store.Store, v vault.Vault) *HealthHandler {
	return &HealthHandler{st: st, v: v}
}

// Health always returns 200 — used by load balancers.
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Ready checks DB and vault connectivity.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{
		"status": "ok",
		"db":     "ok",
		"vault":  "ok",
	}
	code := http.StatusOK

	if err := h.st.Ping(r.Context()); err != nil {
		resp["status"] = "degraded"
		resp["db"] = "error: " + err.Error()
		code = http.StatusServiceUnavailable
	}

	// Vault readiness: attempt to list services for a sentinel user ID.
	// This just exercises the vault connection without requiring real data.
	if _, err := h.v.List(r.Context(), "_health"); err != nil {
		// List returning ErrNotFound or empty is fine — we just want connectivity.
		if err.Error() != vault.ErrNotFound.Error() {
			resp["status"] = "degraded"
			resp["vault"] = "error: " + err.Error()
			code = http.StatusServiceUnavailable
		}
	}

	writeJSON(w, code, resp)
}

// writeJSON is a shared JSON response helper used across all handlers.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a standard error response.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{
		"error": message,
		"code":  code,
	})
}

// decodeJSON decodes the request body into v.
// On failure it writes a 400 error and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body")
		return false
	}
	return true
}
