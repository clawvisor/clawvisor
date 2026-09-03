package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/notify"
)

// slackInstallService is the reserved ServiceID marking an OAuth state entry
// as a Slack notification install rather than a service connection. The
// callback rejects any state that does not carry it, so a state minted for a
// service OAuth flow cannot be replayed against the Slack installer.
const slackInstallService = "__slack_notify_install__"

// slackInstallStateTTL bounds how long an install may sit half-finished.
const slackInstallStateTTL = 10 * time.Minute

// SetSlack wires the Slack dependencies. They are optional — a deployment
// without Slack app credentials leaves them nil and every Slack route
// answers 501.
func (h *NotificationsHandler) SetSlack(cfgStore notify.SlackConfigStore, installer notify.SlackInstaller, state OAuthStateStore) {
	h.slackCfg = cfgStore
	h.slackInstaller = installer
	h.oauthState = state
}

func (h *NotificationsHandler) slackReady(w http.ResponseWriter) bool {
	if h.slackCfg == nil || h.slackInstaller == nil || h.oauthState == nil {
		writeError(w, http.StatusNotImplemented, "SLACK_DISABLED",
			"Slack notifications are not enabled on this deployment")
		return false
	}
	return true
}

// slackConfigResponse is the API view of a Slack connection. It never
// carries the bot token.
type slackConfigResponse struct {
	Connected   bool                   `json:"connected"`
	TeamID      string                 `json:"team_id"`
	TeamName    string                 `json:"team_name"`
	ChannelID   string                 `json:"channel_id"`
	ChannelName string                 `json:"channel_name"`
	Installer   string                 `json:"installer_slack_user_id"`
	Approvers   []notify.SlackApprover `json:"approvers"`
}

// SlackInstallURL starts the install flow.
//
// GET /api/notifications/slack/install
// Auth: user JWT
func (h *NotificationsHandler) SlackInstallURL(w http.ResponseWriter, r *http.Request) {
	if !h.slackReady(w) {
		return
	}
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	state, err := randomState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "could not start install")
		return
	}
	// Binding the state to the caller is what stops an attacker from
	// completing an install against someone else's account.
	h.oauthState.StoreOAuth(state, oauthStateEntry{
		UserID:    user.ID,
		ServiceID: slackInstallService,
		ExpiresAt: time.Now().Add(slackInstallStateTTL),
	})

	installURL, err := h.slackInstaller.SlackInstallURL(state)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SLACK_DISABLED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": installURL})
}

// SlackCallback completes the OAuth exchange and redirects back to settings.
//
// GET /api/notifications/slack/callback
// Auth: none — authorization comes from the signed state parameter.
func (h *NotificationsHandler) SlackCallback(w http.ResponseWriter, r *http.Request) {
	if h.slackCfg == nil || h.slackInstaller == nil || h.oauthState == nil {
		http.Error(w, "Slack notifications are not enabled", http.StatusNotImplemented)
		return
	}

	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		h.redirectToSlackSettings(w, r, "error", e)
		return
	}
	code, state := q.Get("code"), q.Get("state")
	if code == "" || state == "" {
		h.redirectToSlackSettings(w, r, "error", "missing_code")
		return
	}

	entry, ok := h.oauthState.LoadAndDeleteOAuth(state)
	if !ok || entry.ServiceID != slackInstallService || entry.UserID == "" {
		h.redirectToSlackSettings(w, r, "error", "invalid_state")
		return
	}
	if !entry.ExpiresAt.IsZero() && time.Now().After(entry.ExpiresAt) {
		h.redirectToSlackSettings(w, r, "error", "expired_state")
		return
	}

	install, err := h.slackInstaller.CompleteSlackInstall(r.Context(), code)
	if err != nil {
		h.redirectToSlackSettings(w, r, "error", "exchange_failed")
		return
	}

	// The workspace is connected but no channel is chosen yet. Persisting
	// with an empty ChannelID would fail validation, so carry the install
	// forward by writing it against a placeholder the UI immediately
	// replaces via SlackSetChannel.
	existing, _ := h.slackCfg.SlackConfig(r.Context(), entry.UserID)
	cfg := notify.SlackConfig{
		BotToken:             install.BotToken,
		TeamID:               install.TeamID,
		TeamName:             install.TeamName,
		ChannelID:            existing.ChannelID,
		ChannelName:          existing.ChannelName,
		InstallerSlackUserID: install.InstallerUserID,
		Approvers:            existing.Approvers,
	}
	if cfg.ChannelID == "" {
		// SaveSlackConfig requires a channel; use a sentinel that the
		// settings UI treats as "pick a channel".
		cfg.ChannelID = slackPendingChannel
	}
	if err := h.slackCfg.SaveSlackConfig(r.Context(), entry.UserID, cfg); err != nil {
		h.redirectToSlackSettings(w, r, "error", "save_failed")
		return
	}

	h.redirectToSlackSettings(w, r, "slack", "connected")
}

// slackPendingChannel marks a workspace that is installed but has no channel
// selected yet.
const slackPendingChannel = "__pending__"

func (h *NotificationsHandler) redirectToSlackSettings(w http.ResponseWriter, r *http.Request, key, val string) {
	base := strings.TrimRight(h.baseURL, "/")
	http.Redirect(w, r, base+"/settings?"+url.Values{key: {val}}.Encode(), http.StatusFound)
}

// SlackConfig returns the current connection.
//
// GET /api/notifications/slack
func (h *NotificationsHandler) SlackConfig(w http.ResponseWriter, r *http.Request) {
	if !h.slackReady(w) {
		return
	}
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	cfg, err := h.slackCfg.SlackConfig(r.Context(), user.ID)
	if err != nil {
		writeJSON(w, http.StatusOK, slackConfigResponse{Connected: false, Approvers: []notify.SlackApprover{}})
		return
	}
	writeJSON(w, http.StatusOK, slackConfigView(cfg))
}

func slackConfigView(cfg notify.SlackConfig) slackConfigResponse {
	approvers := cfg.Approvers
	if approvers == nil {
		approvers = []notify.SlackApprover{}
	}
	channelID, channelName := cfg.ChannelID, cfg.ChannelName
	if channelID == slackPendingChannel {
		channelID, channelName = "", ""
	}
	return slackConfigResponse{
		Connected:   true,
		TeamID:      cfg.TeamID,
		TeamName:    cfg.TeamName,
		ChannelID:   channelID,
		ChannelName: channelName,
		Installer:   cfg.InstallerSlackUserID,
		Approvers:   approvers,
	}
}

// SlackChannels lists selectable channels.
//
// GET /api/notifications/slack/channels
func (h *NotificationsHandler) SlackChannels(w http.ResponseWriter, r *http.Request) {
	if !h.slackReady(w) {
		return
	}
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	channels, err := h.slackInstaller.ListSlackChannels(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "SLACK_ERROR", "could not list channels")
		return
	}
	if channels == nil {
		channels = []notify.SlackChannel{}
	}
	writeJSON(w, http.StatusOK, channels)
}

// SlackSetChannel chooses the approval destination.
//
// PUT /api/notifications/slack/channel
// Body: {"channel_id": "C123", "channel_name": "approvals"}
func (h *NotificationsHandler) SlackSetChannel(w http.ResponseWriter, r *http.Request) {
	if !h.slackReady(w) {
		return
	}
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	var body struct {
		ChannelID   string `json:"channel_id"`
		ChannelName string `json:"channel_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "malformed request body")
		return
	}
	if body.ChannelID == "" || body.ChannelID == slackPendingChannel {
		writeError(w, http.StatusBadRequest, "INVALID_CHANNEL", "channel_id is required")
		return
	}

	cfg, err := h.slackCfg.SlackConfig(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "NOT_CONNECTED", "connect a Slack workspace first")
		return
	}
	cfg.ChannelID = body.ChannelID
	cfg.ChannelName = strings.TrimPrefix(body.ChannelName, "#")
	if err := h.slackCfg.SaveSlackConfig(r.Context(), user.ID, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "could not save channel")
		return
	}
	writeJSON(w, http.StatusOK, slackConfigView(cfg))
}

// SlackSetApprovers replaces the approver allowlist.
//
// PUT /api/notifications/slack/approvers
// Body: {"slack_user_ids": ["U123", "U456"]}
//
// Channel membership does not confer approval rights, so this list (plus the
// installer) is the complete set of people who can resolve a request from
// Slack.
func (h *NotificationsHandler) SlackSetApprovers(w http.ResponseWriter, r *http.Request) {
	if !h.slackReady(w) {
		return
	}
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	var body struct {
		SlackUserIDs []string `json:"slack_user_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "malformed request body")
		return
	}
	const maxApprovers = 50
	if len(body.SlackUserIDs) > maxApprovers {
		writeError(w, http.StatusBadRequest, "TOO_MANY", "too many approvers")
		return
	}

	cfg, err := h.slackCfg.SlackConfig(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "NOT_CONNECTED", "connect a Slack workspace first")
		return
	}

	seen := map[string]bool{}
	approvers := make([]notify.SlackApprover, 0, len(body.SlackUserIDs))
	for _, id := range body.SlackUserIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		// Resolving the name also proves the ID is a real user in this
		// workspace; an unresolvable ID is rejected rather than silently
		// stored as an allowlist entry that can never match.
		name, err := h.slackInstaller.LookupSlackUser(r.Context(), user.ID, id)
		if err != nil {
			writeError(w, http.StatusBadRequest, "UNKNOWN_USER", "not a member of this workspace: "+id)
			return
		}
		approvers = append(approvers, notify.SlackApprover{SlackUserID: id, DisplayName: name})
	}

	cfg.Approvers = approvers
	if err := h.slackCfg.SaveSlackConfig(r.Context(), user.ID, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "could not save approvers")
		return
	}
	writeJSON(w, http.StatusOK, slackConfigView(cfg))
}

// SlackTest posts a test message to the configured channel.
//
// POST /api/notifications/slack/test
func (h *NotificationsHandler) SlackTest(w http.ResponseWriter, r *http.Request) {
	if !h.slackReady(w) || h.notifier == nil {
		return
	}
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if err := h.notifier.SendTestMessage(r.Context(), user.ID); err != nil {
		writeError(w, http.StatusBadGateway, "SLACK_ERROR", "could not post to the channel")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// SlackDisconnect removes the workspace connection.
//
// DELETE /api/notifications/slack
func (h *NotificationsHandler) SlackDisconnect(w http.ResponseWriter, r *http.Request) {
	if !h.slackReady(w) {
		return
	}
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if err := h.slackCfg.DeleteSlackConfig(r.Context(), user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "could not disconnect")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
