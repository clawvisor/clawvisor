package handlers

import (
	"net/http"
	"sort"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// Instance-shared vault entries (spec 04 §C). A shared entry is owned by the
// `_instance` sentinel user; the InstanceAwareVault resolves it as a
// fallback behind a member's personal entry, so a team can share one
// Anthropic key. Writes are admin-only and this is a SECURITY boundary, not
// a convenience gate: a shared entry is injected into every member's
// govern-mode LLM calls, so writing one is an administrative act. These
// routes are wired under RequireAdminOrToken (instance-admin token or JWT
// admin) — a config-write token is 403 INSUFFICIENT_SCOPE.
//
// AAD/crypto rule: because writes pass InstanceUserID as the owner,
// LocalVault seals shared rows under the `_instance|<serviceID>` AAD
// automatically. Promoting a personal credential to shared therefore
// requires Get (decrypt) then Set under `_instance` (re-encrypt) — never a
// DB row-copy, which GCM would reject on read.

// SharedList returns the service IDs of instance-shared vault entries.
//
// GET /api/vault/shared
func (h *VaultHandler) SharedList(w http.ResponseWriter, r *http.Request) {
	if h.vault == nil {
		writeError(w, http.StatusConflict, "VAULT_DISABLED", "vault is not configured")
		return
	}
	ids, err := h.vault.List(r.Context(), store.InstanceUserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list shared vault entries")
		return
	}
	sort.Strings(ids)
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"shared": ids, "total": len(ids)})
}

// SharedPut writes (creates or overwrites) an instance-shared entry.
//
// PUT /api/vault/shared/{serviceID}
// Body: {"credential":"..."}
func (h *VaultHandler) SharedPut(w http.ResponseWriter, r *http.Request) {
	if h.vault == nil {
		writeError(w, http.StatusConflict, "VAULT_DISABLED", "vault is not configured")
		return
	}
	serviceID := strings.TrimSpace(r.PathValue("serviceID"))
	if serviceID == "" || !validManualVaultItemID(serviceID) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid service id")
		return
	}
	var body struct {
		Credential string `json:"credential"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Credential) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "credential is required")
		return
	}
	// Sealed under the `_instance|<serviceID>` AAD by passing InstanceUserID
	// as the owner (never a row-copy — see the AAD rule above).
	if err := h.vault.Set(r.Context(), store.InstanceUserID, serviceID, []byte(body.Credential)); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not write shared vault entry")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "written", "service_id": serviceID})
}

// SharedDelete removes an instance-shared entry.
//
// DELETE /api/vault/shared/{serviceID}
func (h *VaultHandler) SharedDelete(w http.ResponseWriter, r *http.Request) {
	if h.vault == nil {
		writeError(w, http.StatusConflict, "VAULT_DISABLED", "vault is not configured")
		return
	}
	serviceID := strings.TrimSpace(r.PathValue("serviceID"))
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service id is required")
		return
	}
	if err := h.vault.Delete(r.Context(), store.InstanceUserID, serviceID); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete shared vault entry")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "service_id": serviceID})
}
