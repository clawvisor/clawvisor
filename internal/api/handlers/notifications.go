package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/ericlevine/clawvisor/internal/api/middleware"
	"github.com/ericlevine/clawvisor/internal/store"
)

// NotificationsHandler manages per-user notification channel configuration.
type NotificationsHandler struct {
	st store.Store
}

func NewNotificationsHandler(st store.Store) *NotificationsHandler {
	return &NotificationsHandler{st: st}
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
			"config":     cfg.Config,
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

	// Upsert with empty config signals "disabled"; we can't delete a row cleanly
	// without a DeleteNotificationConfig store method, so store empty JSON instead.
	if err := h.st.UpsertNotificationConfig(r.Context(), user.ID, "telegram", json.RawMessage(`{}`)); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not clear notification config")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
