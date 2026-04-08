package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/clawvisor/clawvisor/internal/groupchat"
	"github.com/clawvisor/clawvisor/pkg/notify"
)

// pollingSession tracks one per-user callback polling goroutine.
type pollingSession struct {
	userID   string
	botToken string
	chatID   string
	cancel   context.CancelFunc
	pending  int32 // number of pending callback tokens

	mu          sync.Mutex // protects groupChatID and persistent
	groupChatID string     // group chat ID for observation; empty if not configured
	persistent  bool       // true when group observation is active (don't stop on pending=0)
}

// ensurePolling starts a polling goroutine for the user if one isn't running,
// or increments the pending counter on the existing one.
func (n *Notifier) ensurePolling(userID, botToken, chatID string) {
	key := userID
	if val, loaded := n.pollers.LoadOrStore(key, &pollingSession{
		userID:   userID,
		botToken: botToken,
		chatID:   chatID,
		pending:  1,
	}); loaded {
		// Already running — just increment.
		ps := val.(*pollingSession)
		atomic.AddInt32(&ps.pending, 1)
		return
	}

	// New session — fill in cancel and start goroutine.
	val, _ := n.pollers.Load(key)
	ps := val.(*pollingSession)
	ctx, cancel := context.WithCancel(n.serverCtx)
	ps.cancel = cancel
	go n.pollForCallbacks(ctx, ps)
}

// EnsureGroupObservation starts or upgrades a polling session to persistent
// mode for group chat observation. If a session already exists, it updates the
// groupChatID and sets persistent=true. If none exists, it creates a new
// persistent session and starts polling.
func (n *Notifier) EnsureGroupObservation(userID, botToken, chatID, groupChatID string) {
	key := userID
	if val, loaded := n.pollers.LoadOrStore(key, &pollingSession{
		userID:      userID,
		botToken:    botToken,
		chatID:      chatID,
		groupChatID: groupChatID,
		persistent:  true,
	}); loaded {
		// Already running — upgrade to persistent and set group chat ID.
		ps := val.(*pollingSession)
		ps.mu.Lock()
		ps.groupChatID = groupChatID
		ps.persistent = true
		ps.mu.Unlock()
		return
	}

	// New session — fill in cancel and start goroutine.
	val, _ := n.pollers.Load(key)
	ps := val.(*pollingSession)
	ctx, cancel := context.WithCancel(n.serverCtx)
	ps.cancel = cancel
	go n.pollForCallbacks(ctx, ps)
}

// StopGroupObservation clears the persistent flag and group chat ID.
// If there are no pending callback tokens, the polling loop is stopped.
func (n *Notifier) StopGroupObservation(userID string) {
	val, ok := n.pollers.Load(userID)
	if !ok {
		return
	}
	ps := val.(*pollingSession)
	ps.mu.Lock()
	ps.persistent = false
	ps.groupChatID = ""
	ps.mu.Unlock()
	if atomic.LoadInt32(&ps.pending) <= 0 {
		ps.cancel()
		n.pollers.Delete(userID)
	}
}

// DecrementPolling decrements the pending count and stops polling if ≤0
// and the session is not persistent (group observation active).
func (n *Notifier) DecrementPolling(userID string) {
	val, ok := n.pollers.Load(userID)
	if !ok {
		return
	}
	ps := val.(*pollingSession)
	ps.mu.Lock()
	persistent := ps.persistent
	ps.mu.Unlock()
	if atomic.AddInt32(&ps.pending, -1) <= 0 && !persistent {
		ps.cancel()
		n.pollers.Delete(userID)
	}
}

// BootstrapGroupObservation reads all Telegram notification configs from the
// store and starts persistent polling for any that have a group_chat_id
// configured. Called once at server startup.
func (n *Notifier) BootstrapGroupObservation(ctx context.Context) {
	configs, err := n.store.ListNotificationConfigsByChannel(ctx, "telegram")
	if err != nil {
		slog.Default().Warn("bootstrap group observation: list configs failed", "err", err)
		return
	}
	for _, nc := range configs {
		var cfg telegramCfg
		if err := json.Unmarshal(nc.Config, &cfg); err != nil {
			continue
		}
		if cfg.GroupChatID == "" || cfg.BotToken == "" || cfg.ChatID == "" {
			continue
		}
		n.EnsureGroupObservation(nc.UserID, cfg.BotToken, cfg.ChatID, cfg.GroupChatID)
		slog.Default().Info("group observation bootstrapped", "user_id", nc.UserID)
	}
}

// pollForCallbacks long-polls getUpdates for callback_query and group message updates.
func (n *Notifier) pollForCallbacks(ctx context.Context, ps *pollingSession) {
	defer func() {
		n.pollers.Delete(ps.userID)
	}()

	logger := slog.Default()

	// Drain old updates to get current offset.
	updates, err := n.getUpdates(ctx, ps.botToken, 0, 0)
	if err != nil {
		logger.Warn("callback polling: drain failed", "user_id", ps.userID, "err", err)
		return
	}
	var offset int
	if len(updates) > 0 {
		offset = updates[len(updates)-1].UpdateID + 1
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		updates, err := n.getUpdates(ctx, ps.botToken, offset, 10)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(2 * time.Second)
			continue
		}

		ps.mu.Lock()
		gcid := ps.groupChatID
		ps.mu.Unlock()

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.CallbackQuery != nil {
				n.handleCallbackQuery(ctx, ps, u.CallbackQuery)
			}
			if u.Message != nil && gcid != "" {
				n.handleGroupMessage(ctx, ps, u.Message, gcid)
			}
			if u.MyChatMember != nil {
				n.handleChatMemberUpdate(ctx, ps, u.MyChatMember)
			}
		}
	}
}

// handleChatMemberUpdate processes a my_chat_member update, detecting when the
// bot has been added to a group or supergroup and storing it as a pending group.
func (n *Notifier) handleChatMemberUpdate(ctx context.Context, ps *pollingSession, cm *chatMemberUpdate) {
	// Only care about the bot being added (member or administrator).
	if cm.NewMember.Status != "member" && cm.NewMember.Status != "administrator" {
		return
	}
	// Only track groups and supergroups.
	if cm.Chat.Type != "group" && cm.Chat.Type != "supergroup" {
		return
	}

	chatID := fmt.Sprintf("%d", cm.Chat.ID)
	n.AddPendingGroup(ps.userID, notify.PendingGroup{
		ChatID:     chatID,
		Title:      cm.Chat.Title,
		Type:       cm.Chat.Type,
		DetectedAt: time.Now(),
	})

	slog.Default().Info("telegram bot added to group",
		"user_id", ps.userID, "chat_id", chatID, "title", cm.Chat.Title)

	// Send a DM to the user's private chat.
	text := fmt.Sprintf("🤖 I was added to <b>%s</b>. Enable group observation from Settings.",
		html.EscapeString(cm.Chat.Title))
	n.sendMessage(ctx, ps.botToken, ps.chatID, text, nil)
}

// handleGroupMessage buffers a message from a monitored group chat.
// All messages are stored (including from agents/bots) so the LLM has
// full conversational context for approval detection.
func (n *Notifier) handleGroupMessage(_ context.Context, ps *pollingSession, msg *telegramMsg, groupChatID string) {
	// Only process messages from the configured group chat.
	if fmt.Sprintf("%d", msg.Chat.ID) != groupChatID {
		return
	}
	if msg.Text == "" {
		return
	}
	if n.msgBuffer != nil {
		n.msgBuffer.Append(groupChatID, groupchat.BufferedMessage{
			Text:       msg.Text,
			SenderID:   fmt.Sprintf("%d", msg.From.ID),
			SenderName: senderDisplayName(msg.From),
			Timestamp:  time.Unix(msg.Date, 0),
		})
		slog.Default().Debug("telegram group message buffered",
			"user_id", ps.userID, "group_chat_id", groupChatID)
	}
}

// senderDisplayName builds a display name from a Telegram user.
func senderDisplayName(u telegramUser) string {
	if u.FirstName != "" {
		return u.FirstName
	}
	if u.Username != "" {
		return u.Username
	}
	return fmt.Sprintf("user:%d", u.ID)
}

// handleCallbackQuery processes a single callback query from a button tap.
func (n *Notifier) handleCallbackQuery(ctx context.Context, ps *pollingSession, cq *callbackQuery) {
	logger := slog.Default()

	// Verify the tap came from the expected chat.
	if cq.From.ID != 0 && fmt.Sprintf("%d", cq.From.ID) != ps.chatID {
		n.answerCallbackQuery(ctx, ps.botToken, cq.ID, "Not authorized.")
		return
	}

	// Parse callback_data: "a:<shortID>" or "d:<shortID>"
	data := cq.Data
	if len(data) < 3 || data[1] != ':' {
		n.answerCallbackQuery(ctx, ps.botToken, cq.ID, "Invalid callback data.")
		return
	}
	prefix := data[0]
	shortID := data[2:]

	var action string
	switch prefix {
	case 'a':
		action = "approve"
	case 'd':
		action = "deny"
	default:
		n.answerCallbackQuery(ctx, ps.botToken, cq.ID, "Invalid callback data.")
		return
	}

	// Consume the token.
	entry, err := n.cbTokens.Consume(shortID)
	if err != nil {
		var msg string
		switch err {
		case errTokenUsed:
			msg = "Already processed."
		case errTokenExpired:
			msg = "This request has expired."
		default:
			msg = "Token not found."
		}
		n.answerCallbackQuery(ctx, ps.botToken, cq.ID, msg)
		return
	}

	// Send decision to the channel.
	decision := notify.CallbackDecision{
		Type:     entry.Type,
		Action:   action,
		TargetID: entry.TargetID,
		UserID:   entry.UserID,
	}
	select {
	case n.decisionCh <- decision:
	case <-ctx.Done():
		return
	}

	// Answer the callback query with confirmation.
	emoji := "✅"
	actionLabel := "Approved"
	if action == "deny" {
		emoji = "❌"
		actionLabel = "Denied"
	}
	n.answerCallbackQuery(ctx, ps.botToken, cq.ID, emoji+" "+actionLabel)

	// Edit the message to remove buttons and show outcome.
	if cq.Message != nil && cq.Message.MessageID != 0 {
		outcomeText := cq.Message.Text
		if outcomeText == "" {
			outcomeText = "Request"
		}
		// Truncate original text for the update.
		if len(outcomeText) > 500 {
			outcomeText = outcomeText[:500] + "..."
		}
		statusLine := fmt.Sprintf("\n\n%s <b>%s</b>", emoji, actionLabel)
		_ = n.editMessage(ctx, ps.botToken, ps.chatID,
			fmt.Sprintf("%d", cq.Message.MessageID),
			outcomeText+statusLine)
	}

	// Decrement pending count.
	n.DecrementPolling(ps.userID)

	logger.Info("telegram callback processed",
		"user_id", entry.UserID, "type", entry.Type,
		"target_id", entry.TargetID, "action", action)
}

// answerCallbackQuery sends a toast notification in Telegram.
func (n *Notifier) answerCallbackQuery(ctx context.Context, botToken, queryID, text string) {
	payload := map[string]any{
		"callback_query_id": queryID,
		"text":              text,
	}
	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
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
