package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	sqlitestore "github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/notify"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestSendConnectionRequestAddsInlineButtons(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "notify@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	cfg, _ := json.Marshal(map[string]string{
		"bot_token": "test-token",
		"chat_id":   "42",
	})
	if err := st.UpsertNotificationConfig(ctx, user.ID, "telegram", cfg); err != nil {
		t.Fatalf("UpsertNotificationConfig: %v", err)
	}

	serverCtx, cancel := context.WithCancel(context.Background())
	cancel()

	n := New(st, serverCtx)

	var payload struct {
		ReplyMarkup struct {
			InlineKeyboard [][]struct {
				Text         string `json:"text"`
				CallbackData string `json:"callback_data"`
			} `json:"inline_keyboard"`
		} `json:"reply_markup"`
	}

	n.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if !strings.Contains(req.URL.Path, "/sendMessage") {
				t.Fatalf("unexpected telegram API call: %s", req.URL.Path)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("Unmarshal payload: %v", err)
			}
			resp := `{"ok":true,"result":{"message_id":123}}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(resp)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	msgID, err := n.SendConnectionRequest(ctx, notifyConnectionRequest(user.ID))
	if err != nil {
		t.Fatalf("SendConnectionRequest: %v", err)
	}
	if msgID != "123" {
		t.Fatalf("expected message ID 123, got %q", msgID)
	}

	if len(payload.ReplyMarkup.InlineKeyboard) != 1 || len(payload.ReplyMarkup.InlineKeyboard[0]) != 2 {
		t.Fatalf("expected one row with two buttons, got %#v", payload.ReplyMarkup.InlineKeyboard)
	}

	approve := payload.ReplyMarkup.InlineKeyboard[0][0]
	deny := payload.ReplyMarkup.InlineKeyboard[0][1]
	if approve.Text != "✅ Approve Connection" || !strings.HasPrefix(approve.CallbackData, "a:") {
		t.Fatalf("unexpected approve button: %+v", approve)
	}
	if deny.Text != "❌ Deny Connection" || !strings.HasPrefix(deny.CallbackData, "d:") {
		t.Fatalf("unexpected deny button: %+v", deny)
	}
}

func notifyConnectionRequest(userID string) notify.ConnectionRequest {
	return notify.ConnectionRequest{
		ConnectionID: "conn-123",
		UserID:       userID,
		AgentName:    "Claude Code",
		IPAddress:    "127.0.0.1:4321",
		ApproveURL:   "http://example.com/approve",
		DenyURL:      "http://example.com/deny",
	}
}
