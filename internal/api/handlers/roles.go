package handlers

import (
	"errors"
	"net/http"

	"github.com/ericlevine/clawvisor/internal/api/middleware"
	"github.com/ericlevine/clawvisor/internal/store"
)

type RolesHandler struct {
	store store.Store
}

func NewRolesHandler(st store.Store) *RolesHandler {
	return &RolesHandler{store: st}
}

// GET /api/roles
func (h *RolesHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	roles, err := h.store.ListRoles(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list roles")
		return
	}
	if roles == nil {
		roles = []*store.AgentRole{}
	}
	writeJSON(w, http.StatusOK, roles)
}

// POST /api/roles
func (h *RolesHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name is required")
		return
	}

	role, err := h.store.CreateRole(r.Context(), user.ID, body.Name, body.Description)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "CONFLICT", "a role with that name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create role")
		return
	}
	writeJSON(w, http.StatusCreated, role)
}

// PUT /api/roles/{id}
func (h *RolesHandler) Update(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	id := r.PathValue("id")

	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name is required")
		return
	}

	role, err := h.store.UpdateRole(r.Context(), id, user.ID, body.Name, body.Description)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "role not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "CONFLICT", "a role with that name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update role")
		return
	}
	writeJSON(w, http.StatusOK, role)
}

// DELETE /api/roles/{id}
func (h *RolesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	id := r.PathValue("id")

	err := h.store.DeleteRole(r.Context(), id, user.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "role not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete role")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
