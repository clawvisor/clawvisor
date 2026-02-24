// Package telegram implements notify.Notifier using the Telegram Bot API.
// Each Clawvisor instance has one bot token (from config). Per-user chat IDs
// are stored in notification_configs (channel = "telegram", config = {"chat_id":"..."}).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ericlevine/clawvisor/internal/notify"
	"github.com/ericlevine/clawvisor/internal/store"
)

// Notifier sends Telegram messages for approval and service-activation requests.
type Notifier struct {
	token  string
	store  store.Store
	client *http.Client
}

// New creates a Telegram Notifier. token is the Bot API token.
func New(token string, st store.Store) *Notifier {
	return &Notifier{
		token:  token,
		store:  st,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// ── notify.Notifier implementation ───────────────────────────────────────────

func (n *Notifier) SendApprovalRequest(ctx context.Context, req notify.ApprovalRequest) (string, error) {
	chatID, err := n.chatID(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	text := formatApprovalMessage(req)
	keyboard := inlineKeyboard([][]inlineButton{{
		{Text: "✅ Approve", URL: req.ApproveURL},
		{Text: "❌ Deny", URL: req.DenyURL},
	}})

	msgID, err := n.sendMessage(ctx, chatID, text, keyboard)
	if err != nil {
		return "", fmt.Errorf("telegram: send approval request: %w", err)
	}
	return msgID, nil
}

func (n *Notifier) SendActivationRequest(ctx context.Context, req notify.ActivationRequest) error {
	chatID, err := n.chatID(ctx, req.UserID)
	if err != nil {
		return err
	}

	text := fmt.Sprintf(
		"🔔 <b>Clawvisor — Service Activation Required</b>\n\n"+
			"<b>Agent:</b> %s wants to use: <b>%s</b>\n"+
			"<b>Requested action:</b> %s",
		html.EscapeString(req.AgentName),
		html.EscapeString(req.Service),
		html.EscapeString(req.AgentName),
	)
	keyboard := inlineKeyboard([][]inlineButton{{
		{Text: "🔗 Activate " + req.Service, URL: req.ActivateURL},
		{Text: "❌ Deny", URL: req.DenyURL},
	}})

	if _, err := n.sendMessage(ctx, chatID, text, keyboard); err != nil {
		return fmt.Errorf("telegram: send activation request: %w", err)
	}
	return nil
}

func (n *Notifier) SendTaskApprovalRequest(ctx context.Context, req notify.TaskApprovalRequest) (string, error) {
	chatID, err := n.chatID(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	text := formatTaskApprovalMessage(req)
	keyboard := inlineKeyboard([][]inlineButton{{
		{Text: "✅ Approve Task", URL: req.ApproveURL},
		{Text: "❌ Deny Task", URL: req.DenyURL},
	}})

	msgID, err := n.sendMessage(ctx, chatID, text, keyboard)
	if err != nil {
		return "", fmt.Errorf("telegram: send task approval request: %w", err)
	}
	return msgID, nil
}

func (n *Notifier) SendScopeExpansionRequest(ctx context.Context, req notify.ScopeExpansionRequest) (string, error) {
	chatID, err := n.chatID(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	text := formatScopeExpansionMessage(req)
	keyboard := inlineKeyboard([][]inlineButton{{
		{Text: "✅ Approve Expansion", URL: req.ApproveURL},
		{Text: "❌ Deny Expansion", URL: req.DenyURL},
	}})

	msgID, err := n.sendMessage(ctx, chatID, text, keyboard)
	if err != nil {
		return "", fmt.Errorf("telegram: send scope expansion request: %w", err)
	}
	return msgID, nil
}

// ── Message formatting ────────────────────────────────────────────────────────

func formatApprovalMessage(req notify.ApprovalRequest) string {
	var sb strings.Builder
	sb.WriteString("🔔 <b>Clawvisor Approval Request</b>\n\n")
	sb.WriteString(fmt.Sprintf("<b>Agent:</b> %s\n", html.EscapeString(req.AgentName)))
	sb.WriteString(fmt.Sprintf("<b>Service:</b> %s\n", html.EscapeString(req.Service)))
	sb.WriteString(fmt.Sprintf("<b>Action:</b> %s\n", html.EscapeString(req.Action)))
	sb.WriteString(fmt.Sprintf("<b>Time:</b> %s\n", time.Now().UTC().Format("Mon Jan 2 2006, 3:04 PM MST")))

	if len(req.Params) > 0 {
		sb.WriteString("\n<b>Parameters:</b>\n")
		// Sort keys for deterministic output.
		keys := make([]string, 0, len(req.Params))
		for k := range req.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("  %s: %s\n",
				html.EscapeString(k),
				html.EscapeString(paramValue(req.Params[k])),
			))
		}
	}

	if req.Reason != "" {
		sb.WriteString(fmt.Sprintf("\n<b>Agent's stated reason:</b>\n  %q\n",
			html.EscapeString(req.Reason)))
	}

	if req.PolicyReason != "" {
		sb.WriteString(fmt.Sprintf("\n⚠️ %s\n", html.EscapeString(req.PolicyReason)))
	} else {
		sb.WriteString("\n⚠️ No policy covers this action.\n")
	}

	sb.WriteString(fmt.Sprintf("\n<i>Expires in %s</i>", html.EscapeString(req.ExpiresIn)))
	return sb.String()
}

func formatTaskApprovalMessage(req notify.TaskApprovalRequest) string {
	var sb strings.Builder
	sb.WriteString("📋 <b>Clawvisor — Task Approval Request</b>\n\n")
	sb.WriteString(fmt.Sprintf("<b>Agent:</b> %s\n", html.EscapeString(req.AgentName)))
	sb.WriteString(fmt.Sprintf("<b>Purpose:</b> %s\n", html.EscapeString(req.Purpose)))
	sb.WriteString(fmt.Sprintf("<b>Time:</b> %s\n", time.Now().UTC().Format("Mon Jan 2 2006, 3:04 PM MST")))

	sb.WriteString("\n<b>Requested actions:</b>\n")
	for _, a := range req.Actions {
		mode := "auto-execute"
		if !a.AutoExecute {
			mode = "requires per-request approval"
		}
		sb.WriteString(fmt.Sprintf("  • %s:%s (%s)\n",
			html.EscapeString(a.Service),
			html.EscapeString(a.Action),
			mode))
	}

	sb.WriteString(fmt.Sprintf("\n<i>Expires in %s</i>", html.EscapeString(req.ExpiresIn)))
	return sb.String()
}

func formatScopeExpansionMessage(req notify.ScopeExpansionRequest) string {
	var sb strings.Builder
	sb.WriteString("🔄 <b>Clawvisor — Scope Expansion Request</b>\n\n")
	sb.WriteString(fmt.Sprintf("<b>Agent:</b> %s\n", html.EscapeString(req.AgentName)))
	sb.WriteString(fmt.Sprintf("<b>Task:</b> %s\n", html.EscapeString(req.Purpose)))

	mode := "auto-execute"
	if !req.NewAction.AutoExecute {
		mode = "requires per-request approval"
	}
	sb.WriteString(fmt.Sprintf("\n<b>New action:</b> %s:%s (%s)\n",
		html.EscapeString(req.NewAction.Service),
		html.EscapeString(req.NewAction.Action),
		mode))

	if req.Reason != "" {
		sb.WriteString(fmt.Sprintf("\n<b>Reason:</b> %s\n", html.EscapeString(req.Reason)))
	}

	return sb.String()
}

// paramValue converts a param value to a display string, truncated at 100 chars.
func paramValue(v any) string {
	var s string
	switch val := v.(type) {
	case string:
		s = val
	case nil:
		s = ""
	default:
		b, _ := json.Marshal(val)
		s = string(b)
	}
	if len(s) > 100 {
		return s[:97] + "..."
	}
	return s
}

// ── Telegram API calls ────────────────────────────────────────────────────────

type inlineButton struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
}

func inlineKeyboard(rows [][]inlineButton) any {
	type replyMarkup struct {
		InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
	}
	return replyMarkup{InlineKeyboard: rows}
}

// sendMessage calls the Telegram sendMessage API and returns the message ID as a string.
func (n *Notifier) sendMessage(ctx context.Context, chatID, text string, replyMarkup any) (string, error) {
	payload := map[string]any{
		"chat_id":      chatID,
		"text":         text,
		"parse_mode":   "HTML",
		"reply_markup": replyMarkup,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var tResp struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &tResp); err != nil {
		return "", fmt.Errorf("telegram API response: %w", err)
	}
	if !tResp.OK {
		return "", fmt.Errorf("telegram API error: %s", tResp.Description)
	}
	return fmt.Sprintf("%d", tResp.Result.MessageID), nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// telegramCfg is the JSON structure stored in notification_configs for channel="telegram".
type telegramCfg struct {
	ChatID string `json:"chat_id"`
}

func (n *Notifier) chatID(ctx context.Context, userID string) (string, error) {
	nc, err := n.store.GetNotificationConfig(ctx, userID, "telegram")
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("telegram: user %s has no Telegram notification configured", userID)
	}
	if err != nil {
		return "", fmt.Errorf("telegram: fetching config for user %s: %w", userID, err)
	}
	var cfg telegramCfg
	if err := json.Unmarshal(nc.Config, &cfg); err != nil {
		return "", fmt.Errorf("telegram: invalid config for user %s: %w", userID, err)
	}
	if cfg.ChatID == "" {
		return "", fmt.Errorf("telegram: user %s config missing chat_id", userID)
	}
	return cfg.ChatID, nil
}
