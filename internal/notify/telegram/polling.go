package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ericlevine/clawvisor/internal/notify"
)

// pollingSession tracks one per-user callback polling goroutine.
type pollingSession struct {
	userID   string
	botToken string
	chatID   string
	cancel   context.CancelFunc
	pending  int32 // number of pending callback tokens
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

// decrementPolling decrements the pending count and stops polling if ≤0.
func (n *Notifier) decrementPolling(userID string) {
	val, ok := n.pollers.Load(userID)
	if !ok {
		return
	}
	ps := val.(*pollingSession)
	if atomic.AddInt32(&ps.pending, -1) <= 0 {
		ps.cancel()
		n.pollers.Delete(userID)
	}
}

// pollForCallbacks long-polls getUpdates for callback_query updates.
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

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.CallbackQuery != nil {
				n.handleCallbackQuery(ctx, ps, u.CallbackQuery)
			}
		}
	}
}

// handleCallbackQuery processes a single callback query from a button tap.
func (n *Notifier) handleCallbackQuery(ctx context.Context, ps *pollingSession, cq *callbackQuery) {
	logger := slog.Default()

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
	n.decrementPolling(ps.userID)

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
