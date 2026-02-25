// Package telegram implements notify.Notifier using the Telegram Bot API.
// Each user stores their own bot_token and chat_id in notification_configs
// (channel = "telegram", config = {"bot_token":"...", "chat_id":"..."}).
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
	"sync"
	"time"

	"github.com/ericlevine/clawvisor/internal/notify"
	"github.com/ericlevine/clawvisor/internal/store"
)

// Notifier sends Telegram messages for approval and service-activation requests.
type Notifier struct {
	store         store.Store
	client        *http.Client
	pairingClient *http.Client // longer timeout for long-poll getUpdates
	pairings      sync.Map     // pairing ID → *pairingSession

	cbTokens   *callbackTokenStore
	pollers    sync.Map // userID → *pollingSession
	decisionCh chan notify.CallbackDecision
	serverCtx  context.Context
}

// New creates a Telegram Notifier that reads per-user bot tokens from the store.
// serverCtx is the server's top-level context; polling goroutines respect its cancellation.
func New(st store.Store, serverCtx context.Context) *Notifier {
	return &Notifier{
		store:         st,
		client:        &http.Client{Timeout: 10 * time.Second},
		pairingClient: &http.Client{Timeout: 30 * time.Second},
		cbTokens:      newCallbackTokenStore(),
		decisionCh:    make(chan notify.CallbackDecision, 32),
		serverCtx:     serverCtx,
	}
}

// DecisionChannel returns a read-only channel that emits callback decisions
// from inline button taps.
func (n *Notifier) DecisionChannel() <-chan notify.CallbackDecision {
	return n.decisionCh
}

// RunCleanup periodically removes expired/used callback tokens.
func (n *Notifier) RunCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.cbTokens.Cleanup()
		}
	}
}

// ── notify.Notifier implementation ───────────────────────────────────────────

func (n *Notifier) SendApprovalRequest(ctx context.Context, req notify.ApprovalRequest) (string, error) {
	botToken, chatID, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	text := formatApprovalMessage(req)

	// Generate callback tokens for inline buttons.
	approveID, denyID, tokenErr := n.cbTokens.Generate("approval", req.RequestID, req.UserID, chatID, 6*time.Minute)
	var keyboard any
	if tokenErr == nil {
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve", CallbackData: "a:" + approveID},
			{Text: "❌ Deny", CallbackData: "d:" + denyID},
		}})
	} else {
		// Fallback to URL buttons if token generation fails.
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve", URL: req.ApproveURL},
			{Text: "❌ Deny", URL: req.DenyURL},
		}})
	}

	msgID, err := n.sendMessage(ctx, botToken, chatID, text, keyboard)
	if err != nil {
		return "", fmt.Errorf("telegram: send approval request: %w", err)
	}

	if tokenErr == nil {
		n.ensurePolling(req.UserID, botToken, chatID)
	}

	return msgID, nil
}

func (n *Notifier) SendActivationRequest(ctx context.Context, req notify.ActivationRequest) error {
	botToken, chatID, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return err
	}

	text := fmt.Sprintf(
		"🔔 <b>Clawvisor — Service Activation Required</b>\n\n"+
			"<b>Agent:</b> %s wants to use: <b>%s</b>\n"+
			"<b>Requested action:</b> %s",
		html.EscapeString(req.AgentName),
		html.EscapeString(req.Service),
		html.EscapeString(req.Service),
	)
	keyboard := inlineKeyboard([][]inlineButton{{
		{Text: "🔗 Activate " + req.Service, URL: req.ActivateURL},
		{Text: "❌ Deny", URL: req.DenyURL},
	}})

	if _, err := n.sendMessage(ctx, botToken, chatID, text, keyboard); err != nil {
		return fmt.Errorf("telegram: send activation request: %w", err)
	}
	return nil
}

func (n *Notifier) SendTaskApprovalRequest(ctx context.Context, req notify.TaskApprovalRequest) (string, error) {
	botToken, chatID, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	text := formatTaskApprovalMessage(req)

	approveID, denyID, tokenErr := n.cbTokens.Generate("task", req.TaskID, req.UserID, chatID, 6*time.Minute)
	var keyboard any
	if tokenErr == nil {
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve Task", CallbackData: "a:" + approveID},
			{Text: "❌ Deny Task", CallbackData: "d:" + denyID},
		}})
	} else {
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve Task", URL: req.ApproveURL},
			{Text: "❌ Deny Task", URL: req.DenyURL},
		}})
	}

	msgID, err := n.sendMessage(ctx, botToken, chatID, text, keyboard)
	if err != nil {
		return "", fmt.Errorf("telegram: send task approval request: %w", err)
	}

	if tokenErr == nil {
		n.ensurePolling(req.UserID, botToken, chatID)
	}

	return msgID, nil
}

func (n *Notifier) SendScopeExpansionRequest(ctx context.Context, req notify.ScopeExpansionRequest) (string, error) {
	botToken, chatID, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	text := formatScopeExpansionMessage(req)

	approveID, denyID, tokenErr := n.cbTokens.Generate("scope_expansion", req.TaskID, req.UserID, chatID, 6*time.Minute)
	var keyboard any
	if tokenErr == nil {
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve Expansion", CallbackData: "a:" + approveID},
			{Text: "❌ Deny Expansion", CallbackData: "d:" + denyID},
		}})
	} else {
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve Expansion", URL: req.ApproveURL},
			{Text: "❌ Deny Expansion", URL: req.DenyURL},
		}})
	}

	msgID, err := n.sendMessage(ctx, botToken, chatID, text, keyboard)
	if err != nil {
		return "", fmt.Errorf("telegram: send scope expansion request: %w", err)
	}

	if tokenErr == nil {
		n.ensurePolling(req.UserID, botToken, chatID)
	}

	return msgID, nil
}

func (n *Notifier) UpdateMessage(ctx context.Context, userID, messageID, text string) error {
	botToken, chatID, err := n.userConfig(ctx, userID)
	if err != nil {
		return err
	}
	return n.editMessage(ctx, botToken, chatID, messageID, text)
}

func (n *Notifier) SendTestMessage(ctx context.Context, userID string) error {
	botToken, chatID, err := n.userConfig(ctx, userID)
	if err != nil {
		return err
	}
	text := "✅ <b>Clawvisor — Test Message</b>\n\nYour Telegram notifications are working!"
	if _, err := n.sendMessage(ctx, botToken, chatID, text, nil); err != nil {
		return fmt.Errorf("telegram: send test message: %w", err)
	}
	return nil
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
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	CallbackData string `json:"callback_data,omitempty"`
}

func inlineKeyboard(rows [][]inlineButton) any {
	type replyMarkup struct {
		InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
	}
	return replyMarkup{InlineKeyboard: rows}
}

// sendMessage calls the Telegram sendMessage API and returns the message ID as a string.
// If the API rejects the request when a reply_markup is included (e.g. inline keyboard
// buttons with localhost URLs), it retries without the markup so the notification text
// is still delivered.
func (n *Notifier) sendMessage(ctx context.Context, botToken, chatID, text string, replyMarkup any) (string, error) {
	msgID, err := n.doSendMessage(ctx, botToken, chatID, text, replyMarkup)
	if err != nil && replyMarkup != nil {
		// Retry without the inline keyboard — the markup (e.g. button URLs)
		// is the most common reason for Telegram rejecting the request.
		msgID, retryErr := n.doSendMessage(ctx, botToken, chatID, text, nil)
		if retryErr == nil {
			return msgID, nil
		}
		// Both attempts failed; return the original error.
	}
	return msgID, err
}

func (n *Notifier) doSendMessage(ctx context.Context, botToken, chatID, text string, replyMarkup any) (string, error) {
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
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

// editMessage calls the Telegram editMessageText API to update a sent message
// and remove its inline keyboard.
func (n *Notifier) editMessage(ctx context.Context, botToken, chatID, messageID, text string) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "HTML",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var tResp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &tResp); err != nil {
		return fmt.Errorf("telegram editMessageText response: %w", err)
	}
	if !tResp.OK {
		return fmt.Errorf("telegram editMessageText error: %s", tResp.Description)
	}
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// telegramCfg is the JSON structure stored in notification_configs for channel="telegram".
type telegramCfg struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

// userConfig fetches the per-user bot_token and chat_id from the store.
func (n *Notifier) userConfig(ctx context.Context, userID string) (botToken, chatID string, err error) {
	nc, err := n.store.GetNotificationConfig(ctx, userID, "telegram")
	if errors.Is(err, store.ErrNotFound) {
		return "", "", fmt.Errorf("telegram: user %s has no Telegram notification configured", userID)
	}
	if err != nil {
		return "", "", fmt.Errorf("telegram: fetching config for user %s: %w", userID, err)
	}
	var cfg telegramCfg
	if err := json.Unmarshal(nc.Config, &cfg); err != nil {
		return "", "", fmt.Errorf("telegram: invalid config for user %s: %w", userID, err)
	}
	if cfg.BotToken == "" {
		return "", "", fmt.Errorf("telegram: user %s config missing bot_token", userID)
	}
	if cfg.ChatID == "" {
		return "", "", fmt.Errorf("telegram: user %s config missing chat_id", userID)
	}
	return cfg.BotToken, cfg.ChatID, nil
}
