package handlers

import (
	"net/http"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
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
// Body: {"name": "..."}
func (h *AgentsHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var body struct {
		Name               string `json:"name"`
		WithCallbackSecret bool   `json:"with_callback_secret"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name is required")
		return
	}

	rawToken, err := auth.GenerateAgentToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate token")
		return
	}

	agent, err := h.st.CreateAgent(r.Context(), user.ID, body.Name, auth.HashToken(rawToken))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create agent")
		return
	}

	resp := map[string]any{
		"id":         agent.ID,
		"user_id":    agent.UserID,
		"name":       agent.Name,
		"created_at": agent.CreatedAt,
		"token":      rawToken,
	}

	if body.WithCallbackSecret {
		secret, err := auth.GenerateCallbackSecret()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate callback secret")
			return
		}
		if err := h.st.SetAgentCallbackSecret(r.Context(), agent.ID, secret); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not store callback secret")
			return
		}
		resp["callback_secret"] = secret
	}

	// Return the raw token here — it is never stored in plaintext and is shown only once.
	writeJSON(w, http.StatusCreated, resp)
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
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "agent not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete agent")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
