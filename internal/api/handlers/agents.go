package handlers

import (
	"log/slog"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// AgentsHandler manages agent token lifecycle.
type AgentsHandler struct {
	st       store.Store
	eventHub events.EventHub
	logger   *slog.Logger
}

func NewAgentsHandler(st store.Store, eventHub events.EventHub, logger *slog.Logger) *AgentsHandler {
	return &AgentsHandler{st: st, eventHub: eventHub, logger: logger}
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

// RotateToken generates a new token for an existing agent without deleting
// the agent record, preserving the agent ID, tasks, and group pairings.
//
// POST /api/agents/{id}/rotate
// Auth: user JWT
func (h *AgentsHandler) RotateToken(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")

	rawToken, err := auth.GenerateAgentToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate token")
		return
	}

	if err := h.st.RotateAgentToken(r.Context(), id, user.ID, auth.HashToken(rawToken)); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "agent not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not rotate token")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":    id,
		"token": rawToken,
	})
}

// Delete removes an agent by ID. Any active or pending tasks belonging to
// the agent are revoked before the agent record is deleted.
//
// DELETE /api/agents/{id}
// Auth: user JWT
func (h *AgentsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")

	// Revoke all active/pending tasks for this agent before deleting.
	revokedCount, err := h.st.RevokeTasksByAgent(ctx, id, user.ID)
	if err != nil {
		h.logger.Warn("failed to revoke tasks for agent", "err", err, "agent_id", id)
	}

	if err := h.st.DeleteAgent(ctx, id, user.ID); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "agent not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete agent")
		return
	}

	if revokedCount > 0 {
		h.eventHub.Publish(user.ID, events.Event{Type: "tasks"})
		h.eventHub.Publish(user.ID, events.Event{Type: "queue"})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"revoked_tasks": revokedCount,
	})
}
