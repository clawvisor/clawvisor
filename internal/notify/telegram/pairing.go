package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// pairingSession is the internal mutable state for a pairing attempt.
type pairingSession struct {
	ID          string
	UserID      string
	BotToken    string
	BotUsername string
	Code        string // 8-char uppercase alphanumeric
	ChatID      string // set when /start is received
	Status      string // polling | ready | confirmed | expired
	CreatedAt   time.Time
	cancel      context.CancelFunc
}

// Telegram API response types for pairing.
type getMeResponse struct {
	OK     bool `json:"ok"`
	Result struct {
		Username string `json:"username"`
	} `json:"result"`
	Description string `json:"description"`
}

type telegramUpdate struct {
	UpdateID      int               `json:"update_id"`
	Message       *telegramMsg      `json:"message"`
	CallbackQuery *callbackQuery    `json:"callback_query"`
	MyChatMember  *chatMemberUpdate `json:"my_chat_member"`
}

type telegramMsg struct {
	MessageID int64        `json:"message_id"`
	Text      string       `json:"text"`
	Chat      telegramChat `json:"chat"`
	From      telegramUser `json:"from"`
	Date      int64        `json:"date"` // Unix timestamp
}

type telegramChat struct {
	ID    int64  `json:"id"`
	Title string `json:"title,omitempty"`
	Type  string `json:"type,omitempty"` // "group", "supergroup", "channel", "private"
}

// chatMemberUpdate represents a Telegram my_chat_member update, sent when
// the bot's membership status changes in a chat (e.g., added to a group).
type chatMemberUpdate struct {
	Chat      telegramChat     `json:"chat"`
	From      telegramUser     `json:"from"`
	NewMember chatMemberStatus `json:"new_chat_member"`
}

type chatMemberStatus struct {
	Status string       `json:"status"` // "member", "administrator", "left", "kicked"
	User   telegramUser `json:"user"`
}

// callbackQuery represents a Telegram callback_query from an inline button tap.
type callbackQuery struct {
	ID      string          `json:"id"`
	From    telegramUser    `json:"from"`
	Message *callbackMessage `json:"message"`
	Data    string          `json:"data"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name,omitempty"`
	Username  string `json:"username,omitempty"`
	// IsBot is supplied by the Telegram API on every user object. We use
	// it to set BufferedMessage.Role correctly so bot chatter can't be
	// mistaken for human approval by the LLM verifier.
	IsBot bool `json:"is_bot,omitempty"`
}

type callbackMessage struct {
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
}

const pairingTimeout = 5 * time.Minute

// StartPairing validates the bot token, generates a pairing code, and starts
// long-polling for the user's /start message.
func (n *Notifier) StartPairing(ctx context.Context, userID, botToken string) (*notify.PairingSession, error) {
	// Cancel any existing pairing for this user.
	n.pairings.Range(func(key, val any) bool {
		s := val.(*pairingSession)
		if s.UserID == userID {
			s.cancel()
			n.pairings.Delete(key)
		}
		return true
	})

	// Cancel any active callback poller for this user — Telegram only allows
	// one getUpdates consumer per bot token at a time.
	if val, ok := n.pollers.Load(userID); ok {
		ps := val.(*pollingSession)
		ps.cancel()
		n.pollers.Delete(userID)
	}

	// Validate bot token via getMe.
	username, err := n.getMe(ctx, botToken)
	if err != nil {
		return nil, fmt.Errorf("invalid bot token: %w", err)
	}

	// Delete any existing webhook so getUpdates works.
	if err := n.deleteWebhook(ctx, botToken); err != nil {
		return nil, fmt.Errorf("could not clear webhook: %w", err)
	}

	code := generatePairingCode()
	id := generatePairingID()
	pollCtx, cancel := context.WithTimeout(context.Background(), pairingTimeout)

	s := &pairingSession{
		ID:          id,
		UserID:      userID,
		BotToken:    botToken,
		BotUsername: username,
		Code:        code,
		Status:      "polling",
		CreatedAt:   time.Now(),
		cancel:      cancel,
	}
	n.pairings.Store(id, s)

	go n.pollForStart(pollCtx, s)

	return &notify.PairingSession{
		ID:          s.ID,
		UserID:      s.UserID,
		BotUsername: s.BotUsername,
		Status:      s.Status,
		ExpiresAt:   s.CreatedAt.Add(pairingTimeout),
	}, nil
}

// PairingStatus returns the current state of a pairing session.
func (n *Notifier) PairingStatus(pairingID string) (*notify.PairingSession, error) {
	val, ok := n.pairings.Load(pairingID)
	if !ok {
		return nil, fmt.Errorf("pairing session not found")
	}
	s := val.(*pairingSession)
	return &notify.PairingSession{
		ID:          s.ID,
		UserID:      s.UserID,
		BotUsername: s.BotUsername,
		Status:      s.Status,
		ExpiresAt:   s.CreatedAt.Add(pairingTimeout),
	}, nil
}

// ConfirmPairing validates the code and persists the Telegram config.
func (n *Notifier) ConfirmPairing(ctx context.Context, pairingID, code string) error {
	val, ok := n.pairings.Load(pairingID)
	if !ok {
		return fmt.Errorf("pairing session not found")
	}
	s := val.(*pairingSession)

	if s.Status == "expired" {
		return fmt.Errorf("pairing session expired")
	}
	if s.Status == "confirmed" {
		return fmt.Errorf("pairing session already confirmed")
	}
	if s.Status != "ready" {
		return fmt.Errorf("pairing not ready — send /start to the bot first")
	}
	if !strings.EqualFold(s.Code, code) {
		return fmt.Errorf("invalid pairing code")
	}

	// Save to store.
	cfgBytes, err := json.Marshal(map[string]string{
		"bot_token": s.BotToken,
		"chat_id":   s.ChatID,
	})
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := n.store.UpsertNotificationConfig(ctx, s.UserID, "telegram", cfgBytes); err != nil {
		return fmt.Errorf("save notification config: %w", err)
	}

	s.Status = "confirmed"
	s.cancel()

	// Clean up after a short delay.
	go func() {
		time.Sleep(60 * time.Second)
		n.pairings.Delete(pairingID)
	}()

	return nil
}

// CancelPairing stops the polling goroutine and removes the session.
func (n *Notifier) CancelPairing(pairingID string) {
	val, ok := n.pairings.Load(pairingID)
	if !ok {
		return
	}
	s := val.(*pairingSession)
	s.cancel()
	n.pairings.Delete(pairingID)
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// getMe calls the Telegram getMe API to validate a bot token and return its username.
func (n *Notifier) getMe(ctx context.Context, botToken string) (string, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var r getMeResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("parse getMe response: %w", err)
	}
	if !r.OK {
		return "", fmt.Errorf("getMe failed: %s", r.Description)
	}
	return r.Result.Username, nil
}

// deleteWebhook clears any webhook so getUpdates works.
func (n *Notifier) deleteWebhook(ctx context.Context, botToken string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/deleteWebhook", botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return nil
}

// pollForStart long-polls getUpdates for a /start command, then sends the
// pairing code reply and sets the session to "ready".
func (n *Notifier) pollForStart(ctx context.Context, s *pairingSession) {
	defer func() {
		// On context expiry, mark as expired if not already resolved.
		if s.Status == "polling" {
			s.Status = "expired"
			go func() {
				time.Sleep(60 * time.Second)
				n.pairings.Delete(s.ID)
			}()
		}
	}()

	// Drain old updates to get the current offset.
	updates, err := n.getUpdates(ctx, s.BotToken, 0, 0)
	if err != nil {
		return
	}
	var offset int
	if len(updates) > 0 {
		offset = updates[len(updates)-1].UpdateID + 1
	}

	// Long-poll for /start.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		updates, err := n.getUpdates(ctx, s.BotToken, offset, 10)
		if err != nil {
			// Context cancelled is expected on timeout.
			if ctx.Err() != nil {
				return
			}
			time.Sleep(2 * time.Second)
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message == nil {
				continue
			}
			text := strings.TrimSpace(u.Message.Text)
			if text == "/start" || strings.HasPrefix(text, "/start ") {
				chatID := fmt.Sprintf("%d", u.Message.Chat.ID)
				s.ChatID = chatID
				s.Status = "ready"

				// Send pairing code reply.
				replyText := fmt.Sprintf("Your Telegram user id: %s\n\nPairing code: %s", chatID, s.Code)
				n.sendPairingReply(ctx, s.BotToken, chatID, replyText)
				return
			}
		}
	}
}

// getUpdates calls the Telegram getUpdates API.
func (n *Notifier) getUpdates(ctx context.Context, botToken string, offset, timeout int) ([]telegramUpdate, error) {
	payload := map[string]any{
		"offset":  offset,
		"timeout": timeout,
		"limit":   100,
	}
	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates", botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.pairingClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var r struct {
		OK     bool             `json:"ok"`
		Result []telegramUpdate `json:"result"`
		Description string      `json:"description"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return nil, fmt.Errorf("parse getUpdates response: %w", err)
	}
	if !r.OK {
		return nil, fmt.Errorf("getUpdates failed: %s", r.Description)
	}
	return r.Result, nil
}

// sendPairingReply sends a plain text message to the user during pairing.
func (n *Notifier) sendPairingReply(ctx context.Context, botToken, chatID, text string) {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if req == nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
}

// generatePairingCode returns an 8-character uppercase alphanumeric string.
func generatePairingCode() string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = chars[b[i]%byte(len(chars))]
	}
	return string(b)
}

// generatePairingID returns a short random hex ID for the pairing session.
func generatePairingID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// Compile-time check that *Notifier implements notify.TelegramPairer.
var _ notify.TelegramPairer = (*Notifier)(nil)

// Ensure store import is used (needed for UpsertNotificationConfig).
var _ store.Store
