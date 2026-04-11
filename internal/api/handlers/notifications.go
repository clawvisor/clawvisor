package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// sanitizeNotificationConfig redacts secret fields (bot_token) from a
// notification config before returning it to the browser.
func sanitizeNotificationConfig(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if tok, ok := m["bot_token"].(string); ok && len(tok) > 4 {
		m["bot_token"] = "***" + tok[len(tok)-4:]
	} else if ok {
		m["bot_token"] = "***"
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// NotificationsHandler manages per-user notification channel configuration.
type NotificationsHandler struct {
	st             store.Store
	notifier       notify.Notifier                 // may be nil
	pairer         notify.TelegramPairer            // may be nil
	groupObs       notify.GroupObserver             // may be nil
	groupDetector  notify.GroupDetector             // may be nil
	agentPairer    notify.AgentGroupPairer          // may be nil
	groupValidator notify.GroupMembershipValidator  // may be nil
	baseURL        string
}

func NewNotificationsHandler(st store.Store, notifier notify.Notifier, pairer notify.TelegramPairer, groupObs notify.GroupObserver, groupDetector notify.GroupDetector, agentPairer notify.AgentGroupPairer, groupValidator notify.GroupMembershipValidator, baseURL string) *NotificationsHandler {
	return &NotificationsHandler{st: st, notifier: notifier, pairer: pairer, groupObs: groupObs, groupDetector: groupDetector, agentPairer: agentPairer, groupValidator: groupValidator, baseURL: baseURL}
}

// List returns all notification configs for the authenticated user.
//
// GET /api/notifications
// Auth: user JWT
func (h *NotificationsHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	// Fetch the two currently-supported channels; omit missing ones gracefully.
	var configs []map[string]any
	for _, channel := range []string{"telegram"} {
		cfg, err := h.st.GetNotificationConfig(r.Context(), user.ID, channel)
		if err != nil {
			continue // not configured — skip
		}
		configs = append(configs, map[string]any{
			"channel":    cfg.Channel,
			"config":     sanitizeNotificationConfig(cfg.Config),
			"created_at": cfg.CreatedAt,
			"updated_at": cfg.UpdatedAt,
		})
	}
	if configs == nil {
		configs = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, configs)
}

// UpsertTelegram saves (or replaces) the Telegram notification config.
//
// PUT /api/notifications/telegram
// Auth: user JWT
// Body: {"bot_token": "...", "chat_id": "..."}
func (h *NotificationsHandler) UpsertTelegram(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var body struct {
		BotToken string `json:"bot_token"`
		ChatID   string `json:"chat_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.BotToken == "" || body.ChatID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "bot_token and chat_id are required")
		return
	}

	cfgBytes, err := json.Marshal(map[string]string{
		"bot_token": body.BotToken,
		"chat_id":   body.ChatID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not encode config")
		return
	}

	if err := h.st.UpsertNotificationConfig(r.Context(), user.ID, "telegram", cfgBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save notification config")
		return
	}

	cfg, err := h.st.GetNotificationConfig(r.Context(), user.ID, "telegram")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not retrieve saved config")
		return
	}
	cfg.Config = sanitizeNotificationConfig(cfg.Config)
	writeJSON(w, http.StatusOK, cfg)
}

// DeleteTelegram removes the Telegram notification config.
//
// DELETE /api/notifications/telegram
// Auth: user JWT
func (h *NotificationsHandler) DeleteTelegram(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	if err := h.st.DeleteNotificationConfig(r.Context(), user.ID, "telegram"); err != nil {
		if err == store.ErrNotFound {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete notification config")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// TestTelegram sends a test message using the user's saved config.
//
// POST /api/notifications/telegram/test
// Auth: user JWT
func (h *NotificationsHandler) TestTelegram(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	if h.notifier == nil {
		writeError(w, http.StatusServiceUnavailable, "NOTIFIER_UNAVAILABLE", "notification service not available")
		return
	}

	if err := h.notifier.SendTestMessage(r.Context(), user.ID); err != nil {
		writeError(w, http.StatusBadRequest, "TEST_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// StartPairing begins a Telegram bot pairing flow.
//
// POST /api/notifications/telegram/pair
// Auth: user JWT
// Body: {"bot_token": "..."}
func (h *NotificationsHandler) StartPairing(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.pairer == nil {
		writeError(w, http.StatusServiceUnavailable, "PAIRER_UNAVAILABLE", "pairing service not available")
		return
	}

	var body struct {
		BotToken string `json:"bot_token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.BotToken == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "bot_token is required")
		return
	}

	session, err := h.pairer.StartPairing(r.Context(), user.ID, body.BotToken)
	if err != nil {
		writeError(w, http.StatusBadRequest, "PAIRING_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// PairingStatus returns the current state of a pairing session.
//
// GET /api/notifications/telegram/pair/{pairing_id}
// Auth: user JWT
func (h *NotificationsHandler) PairingStatus(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.pairer == nil {
		writeError(w, http.StatusServiceUnavailable, "PAIRER_UNAVAILABLE", "pairing service not available")
		return
	}

	pairingID := r.PathValue("pairing_id")
	if pairingID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "pairing_id is required")
		return
	}

	session, err := h.pairer.PairingStatus(pairingID)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if session.UserID != user.ID {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "pairing session not found")
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// ConfirmPairing validates the pairing code and saves the Telegram config.
//
// POST /api/notifications/telegram/pair/{pairing_id}/confirm
// Auth: user JWT
// Body: {"code": "..."}
func (h *NotificationsHandler) ConfirmPairing(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.pairer == nil {
		writeError(w, http.StatusServiceUnavailable, "PAIRER_UNAVAILABLE", "pairing service not available")
		return
	}

	pairingID := r.PathValue("pairing_id")
	if pairingID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "pairing_id is required")
		return
	}

	// Verify ownership before confirming.
	session, err := h.pairer.PairingStatus(pairingID)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if session.UserID != user.ID {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "pairing session not found")
		return
	}

	var body struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Code == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "code is required")
		return
	}

	if err := h.pairer.ConfirmPairing(r.Context(), pairingID, body.Code); err != nil {
		writeError(w, http.StatusBadRequest, "CONFIRM_FAILED", err.Error())
		return
	}

	// Return the saved config.
	cfg, err := h.st.GetNotificationConfig(r.Context(), user.ID, "telegram")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "confirmed"})
		return
	}
	cfg.Config = sanitizeNotificationConfig(cfg.Config)
	writeJSON(w, http.StatusOK, cfg)
}

// UpsertTelegramGroup enables group chat observation for a specific group.
// Creates a row in telegram_groups and starts observation.
//
// POST /api/notifications/telegram/group
// Auth: user JWT
// Body: {"group_chat_id": "...", "title": "..."}
func (h *NotificationsHandler) UpsertTelegramGroup(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var body struct {
		GroupChatID string `json:"group_chat_id"`
		Title       string `json:"title"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.GroupChatID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "group_chat_id is required")
		return
	}

	// Read existing Telegram config to get bot token for observation.
	nc, err := h.st.GetNotificationConfig(r.Context(), user.ID, "telegram")
	if err != nil {
		writeError(w, http.StatusBadRequest, "NOT_CONFIGURED", "Telegram notifications must be configured first")
		return
	}

	// Create the telegram_groups row.
	tg, err := h.st.CreateTelegramGroup(r.Context(), user.ID, body.GroupChatID, body.Title)
	if err != nil {
		if err == store.ErrConflict {
			writeError(w, http.StatusConflict, "ALREADY_EXISTS", "this group is already connected")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save group")
		return
	}

	// Start group observation.
	if h.groupObs != nil {
		var cfgMap map[string]any
		if json.Unmarshal(nc.Config, &cfgMap) == nil {
			botToken, _ := cfgMap["bot_token"].(string)
			chatID, _ := cfgMap["chat_id"].(string)
			h.groupObs.EnsureGroupObservation(user.ID, botToken, chatID, body.GroupChatID)
		}
	}
	// Remove from pending list now that it's been enabled.
	if h.groupDetector != nil {
		h.groupDetector.RemovePendingGroup(user.ID, body.GroupChatID)
	}

	writeJSON(w, http.StatusOK, tg)
}

// DeleteTelegramGroup removes a specific group from observation,
// stops polling for it, and cleans up agent pairings.
//
// DELETE /api/notifications/telegram/groups/active/{group_chat_id}
// Auth: user JWT
func (h *NotificationsHandler) DeleteTelegramGroup(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	groupChatID := r.PathValue("group_chat_id")
	if groupChatID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "group_chat_id is required")
		return
	}

	if err := h.st.DeleteTelegramGroup(r.Context(), user.ID, groupChatID); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete group")
		return
	}

	// Stop group observation and clean up agent pairings.
	if h.groupObs != nil {
		h.groupObs.StopGroupObservation(user.ID, groupChatID)
	}
	if h.agentPairer != nil {
		_ = h.agentPairer.UnpairAgentsForGroup(r.Context(), groupChatID)
	}

	w.WriteHeader(http.StatusNoContent)
}

// DetectTelegramGroups triggers a one-shot scan for groups the bot has been
// added to, and returns the pending groups list.
//
// POST /api/notifications/telegram/groups/detect
// Auth: user JWT
func (h *NotificationsHandler) DetectTelegramGroups(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.groupDetector == nil {
		writeError(w, http.StatusServiceUnavailable, "DETECTOR_UNAVAILABLE", "group detection not available")
		return
	}

	groups, err := h.groupDetector.DetectGroups(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DETECT_FAILED", err.Error())
		return
	}
	if groups == nil {
		groups = []notify.PendingGroup{}
	}
	writeJSON(w, http.StatusOK, groups)
}

// ListTelegramGroups returns the pending groups that have been detected but
// not yet enabled for group observation.
//
// GET /api/notifications/telegram/groups
// Auth: user JWT
func (h *NotificationsHandler) ListTelegramGroups(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.groupDetector == nil {
		writeJSON(w, http.StatusOK, []notify.PendingGroup{})
		return
	}

	groups := h.groupDetector.PendingGroups(user.ID)
	if groups == nil {
		groups = []notify.PendingGroup{}
	}
	writeJSON(w, http.StatusOK, groups)
}

// DismissTelegramGroup removes a pending group without enabling observation.
//
// DELETE /api/notifications/telegram/groups/{chat_id}
// Auth: user JWT
func (h *NotificationsHandler) DismissTelegramGroup(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	chatID := r.PathValue("chat_id")
	if chatID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "chat_id is required")
		return
	}

	if h.groupDetector != nil {
		h.groupDetector.RemovePendingGroup(user.ID, chatID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetAutoApproval toggles auto-approval settings for a specific group.
//
// PUT /api/notifications/telegram/groups/active/{group_chat_id}/auto-approval
// Auth: user JWT
// Body: {"enabled": true, "notify": false}
func (h *NotificationsHandler) SetAutoApproval(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	groupChatID := r.PathValue("group_chat_id")
	if groupChatID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "group_chat_id is required")
		return
	}

	var body struct {
		Enabled bool  `json:"enabled"`
		Notify  *bool `json:"notify,omitempty"` // nil = don't change
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	if err := h.st.UpdateTelegramGroupAutoApproval(r.Context(), user.ID, groupChatID, body.Enabled, body.Notify); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update auto-approval settings")
		return
	}

	tg, err := h.st.GetTelegramGroup(r.Context(), user.ID, groupChatID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
		return
	}
	writeJSON(w, http.StatusOK, tg)
}

// CreateGroupPairing creates a new pairing session for a specific group.
//
// POST /api/notifications/telegram/groups/active/{group_chat_id}/pair
// Auth: user JWT
func (h *NotificationsHandler) CreateGroupPairing(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.agentPairer == nil || h.baseURL == "" {
		writeError(w, http.StatusServiceUnavailable, "PAIRER_UNAVAILABLE", "agent-group pairing not available")
		return
	}

	groupChatID := r.PathValue("group_chat_id")
	if groupChatID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "group_chat_id is required")
		return
	}

	// Verify the group exists for this user.
	if _, err := h.st.GetTelegramGroup(r.Context(), user.ID, groupChatID); err != nil {
		writeError(w, http.StatusBadRequest, "NO_GROUP", "group not found")
		return
	}

	sessionID, err := h.agentPairer.StartGroupPairing(r.Context(), user.ID, groupChatID, h.baseURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "PAIRING_FAILED", err.Error())
		return
	}

	pairingURL := fmt.Sprintf("%s/api/notifications/telegram/groups/pair/%s", h.baseURL, sessionID)
	pairingPath := fmt.Sprintf("/api/notifications/telegram/groups/pair/%s", sessionID)
	instruction := fmt.Sprintf(
		"To pair with Clawvisor for auto-approval, send:\n\n"+
			"curl -X POST <your clawvisor base url>%s \\\n  -H \"Authorization: Bearer <your clawvisor agent token>\"\n\n"+
			"This session expires in 5 minutes.",
		pairingPath)

	writeJSON(w, http.StatusOK, map[string]string{
		"session_id":  sessionID,
		"pairing_url": pairingURL,
		"instruction": instruction,
	})
}

// PairAgentToGroup completes an agent-to-group pairing session.
// The agent calls this endpoint after seeing the pairing message in the group.
//
// POST /api/notifications/telegram/groups/pair/{session_id}
// Auth: agent bearer token
func (h *NotificationsHandler) PairAgentToGroup(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "agent authentication required")
		return
	}
	if h.agentPairer == nil {
		writeError(w, http.StatusServiceUnavailable, "PAIRER_UNAVAILABLE", "agent-group pairing not available")
		return
	}

	sessionID := r.PathValue("session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "session_id is required")
		return
	}

	if err := h.agentPairer.CompleteGroupPairing(r.Context(), sessionID, agent.ID, agent.UserID); err != nil {
		writeError(w, http.StatusBadRequest, "PAIRING_FAILED", err.Error())
		return
	}

	groupChatID, _ := h.agentPairer.AgentGroupChatID(r.Context(), agent.ID)
	writeJSON(w, http.StatusOK, map[string]string{
		"status":        "paired",
		"group_chat_id": groupChatID,
	})
}

// ListPairedAgents returns the agents paired to a specific group chat.
//
// GET /api/notifications/telegram/groups/active/{group_chat_id}/agents
// Auth: user JWT
func (h *NotificationsHandler) ListPairedAgents(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	groupChatID := r.PathValue("group_chat_id")
	if groupChatID == "" || h.agentPairer == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}

	pairedIDs, _ := h.agentPairer.PairedAgentIDs(r.Context(), groupChatID)
	if len(pairedIDs) == 0 {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	pairedSet := make(map[string]bool, len(pairedIDs))
	for _, id := range pairedIDs {
		pairedSet[id] = true
	}

	allAgents, err := h.st.ListAgents(r.Context(), user.ID)
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}

	type pairedAgent struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var agents []pairedAgent
	for _, a := range allAgents {
		if pairedSet[a.ID] {
			agents = append(agents, pairedAgent{ID: a.ID, Name: a.Name})
		}
	}
	if agents == nil {
		agents = []pairedAgent{}
	}
	writeJSON(w, http.StatusOK, agents)
}

// AddGroupManually validates that the bot is a member of the specified group
// via the Telegram API, then creates a telegram_groups row and starts observation.
//
// POST /api/notifications/telegram/groups/manual
// Auth: user JWT
// Body: {"group_chat_id": "..."}
func (h *NotificationsHandler) AddGroupManually(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.groupValidator == nil {
		writeError(w, http.StatusServiceUnavailable, "VALIDATOR_UNAVAILABLE", "Telegram notifications must be configured before adding groups")
		return
	}

	var body struct {
		GroupChatID string `json:"group_chat_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.GroupChatID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "group_chat_id is required")
		return
	}

	// Validate that the bot is actually in this group.
	info, err := h.groupValidator.ValidateGroupMembership(r.Context(), user.ID, body.GroupChatID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "NOT_A_MEMBER", err.Error())
		return
	}

	// Read existing Telegram config to get bot token for observation.
	nc, err := h.st.GetNotificationConfig(r.Context(), user.ID, "telegram")
	if err != nil {
		writeError(w, http.StatusBadRequest, "NOT_CONFIGURED", "Telegram notifications must be configured first")
		return
	}

	// Create the telegram_groups row.
	tg, err := h.st.CreateTelegramGroup(r.Context(), user.ID, info.ChatID, info.Title)
	if err != nil {
		if err == store.ErrConflict {
			writeError(w, http.StatusConflict, "ALREADY_EXISTS", "this group is already connected")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save group")
		return
	}

	// Start group observation.
	if h.groupObs != nil {
		var cfgMap map[string]any
		if json.Unmarshal(nc.Config, &cfgMap) == nil {
			botToken, _ := cfgMap["bot_token"].(string)
			chatID, _ := cfgMap["chat_id"].(string)
			h.groupObs.EnsureGroupObservation(user.ID, botToken, chatID, info.ChatID)
		}
	}
	// Remove from pending list if it was there.
	if h.groupDetector != nil {
		h.groupDetector.RemovePendingGroup(user.ID, info.ChatID)
	}

	writeJSON(w, http.StatusOK, tg)
}

// ListActiveGroups returns all connected telegram groups for the user.
//
// GET /api/notifications/telegram/groups/active
// Auth: user JWT
func (h *NotificationsHandler) ListActiveGroups(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	groups, err := h.st.ListTelegramGroups(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list groups")
		return
	}
	if groups == nil {
		groups = []*store.TelegramGroup{}
	}
	writeJSON(w, http.StatusOK, groups)
}
