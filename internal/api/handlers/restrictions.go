package handlers

import (
	"net/http"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/store"
)

// RestrictionsHandler manages hard-block restrictions.
type RestrictionsHandler struct {
	st store.Store
}

func NewRestrictionsHandler(st store.Store) *RestrictionsHandler {
	return &RestrictionsHandler{st: st}
}

// List returns all restrictions for the authenticated user.
//
// GET /api/restrictions
// Auth: user JWT
func (h *RestrictionsHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	restrictions, err := h.st.ListRestrictions(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list restrictions")
		return
	}
	if restrictions == nil {
		restrictions = []*store.Restriction{}
	}
	writeJSON(w, http.StatusOK, restrictions)
}

type createRestrictionRequest struct {
	Service string `json:"service"`
	Action  string `json:"action"`
	Reason  string `json:"reason"`
}

// Create adds a new restriction.
//
// POST /api/restrictions
// Auth: user JWT
func (h *RestrictionsHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var req createRestrictionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Service == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service is required")
		return
	}
	if req.Action == "" {
		req.Action = "*"
	}

	restriction := &store.Restriction{
		UserID:  user.ID,
		Service: req.Service,
		Action:  req.Action,
		Reason:  req.Reason,
	}

	created, err := h.st.CreateRestriction(r.Context(), restriction)
	if err != nil {
		if err == store.ErrConflict {
			writeError(w, http.StatusConflict, "CONFLICT", "restriction already exists for this service/action")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create restriction")
		return
	}

	writeJSON(w, http.StatusCreated, created)
}

// Delete removes a restriction.
//
// DELETE /api/restrictions/{id}
// Auth: user JWT
func (h *RestrictionsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	if err := h.st.DeleteRestriction(r.Context(), id, user.ID); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "restriction not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete restriction")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
