package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/notify"
	"github.com/clawvisor/clawvisor/internal/store"
)

// NotificationsHandler manages per-user notification channel configuration.
type NotificationsHandler struct {
	st       store.Store
	notifier notify.Notifier        // may be nil
	pairer   notify.TelegramPairer  // may be nil
}

func NewNotificationsHandler(st store.Store, notifier notify.Notifier, pairer notify.TelegramPairer) *NotificationsHandler {
	return &NotificationsHandler{st: st, notifier: notifier, pairer: pairer}
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
	writeJSON(w, http.StatusOK, cfg)
}
