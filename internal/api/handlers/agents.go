package handlers

import (
	"errors"
	"net/http"

	"github.com/ericlevine/clawvisor/internal/api/middleware"
	"github.com/ericlevine/clawvisor/internal/store"
)

// AgentsHandler manages agent token lifecycle.
type AgentsHandler struct {
	st store.Store
}

func NewAgentsHandler(st store.Store) *AgentsHandler {
	return &AgentsHandler{st: st}
}

// Create registers a new agent and returns its raw bearer token (shown once).
//
// POST /api/agents
// Auth: user JWT
// Body: {"name": "...", "role_id": "..." (optional)}
func (h *AgentsHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var body struct {
		Name   string  `json:"name"`
		RoleID *string `json:"role_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name is required")
		return
	}

	rawToken, err := generateToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate token")
		return
	}

	agent, err := h.st.CreateAgent(r.Context(), user.ID, body.Name, hashToken(rawToken), body.RoleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create agent")
		return
	}

	// Return the raw token here — it is never stored in plaintext and is shown only once.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         agent.ID,
		"user_id":    agent.UserID,
		"name":       agent.Name,
		"role_id":    agent.RoleID,
		"created_at": agent.CreatedAt,
		"token":      rawToken,
	})
}

// List returns all agents belonging to the authenticated user.
//
// GET /api/agents
// Auth: user JWT
func (h *AgentsHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	agents, err := h.st.ListAgents(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list agents")
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

// UpdateRole changes the role assignment for an existing agent.
//
// PATCH /api/agents/{id}
// Auth: user JWT
// Body: {"role_id": "..." | null}
func (h *AgentsHandler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	var body struct {
		RoleID *string `json:"role_id"` // null = remove role
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	agent, err := h.st.UpdateAgentRole(r.Context(), id, user.ID, body.RoleID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "agent not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update agent")
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// Delete removes an agent by ID.
//
// DELETE /api/agents/{id}
// Auth: user JWT
func (h *AgentsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	if err := h.st.DeleteAgent(r.Context(), id, user.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "agent not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete agent")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
