package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// AgentsHandler manages agent token lifecycle.
type AgentsHandler struct {
	st       store.Store
	eventHub events.EventHub
	logger   *slog.Logger
	cfg      *config.Config
}

func NewAgentsHandler(st store.Store, eventHub events.EventHub, logger *slog.Logger, cfg *config.Config) *AgentsHandler {
	return &AgentsHandler{st: st, eventHub: eventHub, logger: logger, cfg: cfg}
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
	// A pending_verification account cannot mint an agent token until it has
	// proven email possession (invite security rule 2). Token-authenticated
	// callers act as the verified `_instance` user, so headless Terraform /
	// CI enrollment is unaffected.
	if !user.Verified() {
		writeError(w, http.StatusForbidden, "EMAIL_NOT_VERIFIED", "confirm your email before registering an agent")
		return
	}

	var body struct {
		Description string `json:"description"`
		Name        string `json:"name"`
		// WithCallbackSecret is a *bool so we can distinguish:
		//   nil   → field absent — default to true (mint a secret).
		//   true  → explicit opt-in (same as default).
		//   false → explicit opt-out — no secret minted.
		// The default flipped from off to on once unsigned callbacks were
		// recognized as a forgery surface: anyone learning the callback URL
		// could impersonate a "result" to the agent.
		WithCallbackSecret *bool `json:"with_callback_secret"`
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
	if body.Description != "" {
		if err := h.st.UpdateAgentDescription(r.Context(), agent.ID, user.ID, body.Description); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save agent description")
			return
		}
		agent.Description = body.Description
	}

	resp := map[string]any{
		"id":          agent.ID,
		"user_id":     agent.UserID,
		"name":        agent.Name,
		"description": agent.Description,
		"created_at":  agent.CreatedAt,
		"token":       rawToken,
	}

	wantSecret := body.WithCallbackSecret == nil || *body.WithCallbackSecret
	if wantSecret {
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
	if agents == nil {
		agents = []*store.Agent{}
	}
	writeJSON(w, http.StatusOK, agents)
}

func (h *AgentsHandler) GetRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	agent, err := h.loadUserAgent(r, user.ID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "agent not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load agent")
		return
	}
	settings := h.defaultAgentRuntimeSettings(agent.ID)
	if stored, err := h.st.GetAgentRuntimeSettings(r.Context(), agent.ID); err == nil {
		settings = stored
	} else if err != store.ErrNotFound {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load agent runtime settings")
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (h *AgentsHandler) UpdateRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	agent, err := h.loadUserAgent(r, user.ID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "agent not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load agent")
		return
	}
	settings := h.defaultAgentRuntimeSettings(agent.ID)
	if stored, err := h.st.GetAgentRuntimeSettings(r.Context(), agent.ID); err == nil {
		settings = stored
	} else if err != store.ErrNotFound {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load agent runtime settings")
		return
	}
	var body struct {
		RuntimeEnabled                   bool    `json:"runtime_enabled"`
		RuntimeMode                      string  `json:"runtime_mode"`
		StarterProfile                   string  `json:"starter_profile"`
		OutboundCredentialMode           string  `json:"outbound_credential_mode"`
		InjectStoredBearer               bool    `json:"inject_stored_bearer"`
		LiteProxySecretDetectionDisabled *bool   `json:"lite_proxy_secret_detection_disabled"`
		ConversationAutoApproveThreshold *string `json:"conversation_auto_approve_threshold"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	switch body.RuntimeMode {
	case "observe", "enforce":
	default:
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "runtime_mode must be observe or enforce")
		return
	}
	switch body.OutboundCredentialMode {
	case "inherit", "observe", "strict":
	default:
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "outbound_credential_mode must be inherit, observe, or strict")
		return
	}
	if body.StarterProfile == "" {
		body.StarterProfile = "none"
	}
	// Conversation auto-approve threshold is optional in the request
	// body so the existing runtime-settings clients (which don't know
	// about this field) don't accidentally clobber it on unrelated
	// edits. When supplied, it MUST validate under the UI cap — the
	// API is the authoritative gate; the UI dropdown happens to surface
	// the same set but a direct API call cannot bypass.
	if body.ConversationAutoApproveThreshold != nil {
		normalized, err := store.ValidateConversationAutoApproveThreshold(*body.ConversationAutoApproveThreshold, true)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		settings.ConversationAutoApproveThreshold = normalized
	}
	settings.RuntimeEnabled = body.RuntimeEnabled
	settings.RuntimeMode = body.RuntimeMode
	settings.StarterProfile = body.StarterProfile
	settings.OutboundCredentialMode = body.OutboundCredentialMode
	settings.InjectStoredBearer = body.InjectStoredBearer
	if body.LiteProxySecretDetectionDisabled != nil {
		settings.LiteProxySecretDetectionDisabled = *body.LiteProxySecretDetectionDisabled
	}
	if err := h.st.UpsertAgentRuntimeSettings(r.Context(), settings); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save agent runtime settings")
		return
	}
	writeJSON(w, http.StatusOK, settings)
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
		if err == store.ErrConflict {
			// Agent was issued by an MCP/relay flow that bounds the
			// token's lifetime. Rotation here would silently re-issue a
			// token with the same (possibly past) expiry.
			writeError(w, http.StatusConflict, "ROTATION_NOT_ALLOWED",
				"this agent's token has a bounded expiry; re-pair through the original MCP or relay flow to get a fresh token")
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
		h.logger.WarnContext(ctx, "failed to revoke tasks for agent", "err", err, "agent_id", id)
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

func (h *AgentsHandler) loadUserAgent(r *http.Request, userID string) (*store.Agent, error) {
	return loadUserAgent(r.Context(), h.st, userID, r.PathValue("id"))
}

func (h *AgentsHandler) defaultAgentRuntimeSettings(agentID string) *store.AgentRuntimeSettings {
	return defaultAgentRuntimeSettings(h.cfg, agentID)
}

func loadUserAgent(ctx context.Context, st store.Store, userID, agentID string) (*store.Agent, error) {
	agents, err := st.ListAgents(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, agent := range agents {
		if agent.ID == agentID {
			return agent, nil
		}
	}
	return nil, store.ErrNotFound
}

func defaultAgentRuntimeSettings(cfg *config.Config, agentID string) *store.AgentRuntimeSettings {
	runtimeMode := "observe"
	if cfg != nil && !cfg.RuntimePolicy.ObservationModeDefault {
		runtimeMode = "enforce"
	}
	return &store.AgentRuntimeSettings{
		AgentID:                          agentID,
		RuntimeEnabled:                   true,
		RuntimeMode:                      runtimeMode,
		StarterProfile:                   "none",
		OutboundCredentialMode:           "inherit",
		InjectStoredBearer:               cfg != nil && cfg.RuntimePolicy.InjectStoredBearer,
		LiteProxySecretDetectionDisabled: true,
		ConversationAutoApproveThreshold: store.ConversationAutoApproveOff,
	}
}
