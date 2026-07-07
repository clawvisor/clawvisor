package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// serviceConfigResponse is the wire shape the config CRUD endpoints return
// (spec 06b's clawvisor_service_config resource). Config is the opaque JSON
// document stored verbatim per (user, service, alias).
type serviceConfigResponse struct {
	ServiceID string          `json:"service_id"`
	Alias     string          `json:"alias"`
	Config    json.RawMessage `json:"config"`
}

// serviceConfigAliasFromQuery resolves the alias query param, defaulting to
// "default" (matching the activate flows' aliasing).
func serviceConfigAliasFromQuery(r *http.Request) string {
	alias := strings.TrimSpace(r.URL.Query().Get("alias"))
	if alias == "" {
		return "default"
	}
	return alias
}

// GetConfig returns the stored config document for a service/alias.
//
// GET /api/services/{serviceID}/config?alias=default
// Auth: user JWT or instance-admin API token.
func (h *ServicesHandler) GetConfig(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	serviceID := r.PathValue("serviceID")
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service id is required")
		return
	}
	alias := serviceConfigAliasFromQuery(r)
	sc, err := h.st.GetServiceConfig(r.Context(), user.ID, serviceID, alias)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "service config not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load service config")
		return
	}
	writeJSON(w, http.StatusOK, serviceConfigResponse{ServiceID: serviceID, Alias: alias, Config: sc.Config})
}

// PutConfig upserts the config document for a service/alias.
//
// PUT /api/services/{serviceID}/config
// Auth: user JWT or instance-admin API token.
// Body: {"alias":"default","config":{...}}
func (h *ServicesHandler) PutConfig(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	serviceID := r.PathValue("serviceID")
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service id is required")
		return
	}
	// Reject unknown/typo'd service IDs so a config document never persists as
	// an orphan row for a service that does not exist (matching the activation
	// handlers' GetForUser guard).
	if _, ok := h.adapterReg.GetForUser(r.Context(), serviceID, user.ID); !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("service %q not found", serviceID))
		return
	}
	var body struct {
		Alias  string          `json:"alias"`
		Config json.RawMessage `json:"config"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	alias := strings.TrimSpace(body.Alias)
	if alias == "" {
		alias = "default"
	}
	if !validAlias(alias) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters")
		return
	}
	if len(body.Config) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "config is required")
		return
	}
	if !json.Valid(body.Config) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "config must be a valid JSON document")
		return
	}
	if err := h.st.UpsertServiceConfig(r.Context(), user.ID, serviceID, alias, body.Config); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save service config")
		return
	}
	writeJSON(w, http.StatusOK, serviceConfigResponse{ServiceID: serviceID, Alias: alias, Config: body.Config})
}

// DeleteConfig removes the config document for a service/alias.
//
// DELETE /api/services/{serviceID}/config?alias=default
// Auth: user JWT or instance-admin API token.
func (h *ServicesHandler) DeleteConfig(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	serviceID := r.PathValue("serviceID")
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service id is required")
		return
	}
	alias := serviceConfigAliasFromQuery(r)
	if err := h.st.DeleteServiceConfig(r.Context(), user.ID, serviceID, alias); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete service config")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
