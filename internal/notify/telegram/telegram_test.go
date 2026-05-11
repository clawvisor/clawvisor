package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	sqlitestore "github.com/clawvisor/clawvisor/pkg/store/sqlite"
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

// TestCallbackTokenStore_TaskIDRoundTrips guards the symmetric-dedup
// disambiguator for Telegram inline buttons: Generate must persist the
// taskID alongside (type, target_id) and Consume must surface it on the
// returned entry so the decision consumer can route to the correct sibling
// pending approval. Without this, two pending approvals sharing a
// request_id across tasks would emit indistinguishable CallbackDecisions
// and the resolver would hit ErrAmbiguous.
func TestCallbackTokenStore_TaskIDRoundTrips(t *testing.T) {
	t.Parallel()
	s := newCallbackTokenStore()

	approveA, denyA, err := s.Generate("approval", "req-X", "user-1", "task-A", "chat-1", time.Minute)
	if err != nil {
		t.Fatalf("Generate(task-A): %v", err)
	}
	approveB, denyB, err := s.Generate("approval", "req-X", "user-1", "task-B", "chat-1", time.Minute)
	if err != nil {
		t.Fatalf("Generate(task-B): %v", err)
	}
	if approveA == approveB || denyA == denyB {
		t.Fatalf("Generate must mint unique tokens per sibling, got dup")
	}

	gotA, err := s.Consume(approveA)
	if err != nil {
		t.Fatalf("Consume(approveA): %v", err)
	}
	if gotA.TaskID != "task-A" || gotA.TargetID != "req-X" {
		t.Fatalf("Consume(approveA): TaskID=%q TargetID=%q, want task-A/req-X", gotA.TaskID, gotA.TargetID)
	}

	gotB, err := s.Consume(denyB)
	if err != nil {
		t.Fatalf("Consume(denyB): %v", err)
	}
	if gotB.TaskID != "task-B" || gotB.TargetID != "req-X" {
		t.Fatalf("Consume(denyB): TaskID=%q TargetID=%q, want task-B/req-X", gotB.TaskID, gotB.TargetID)
	}

	// Pair-state is shared per (approve, deny) entry: consuming approveA
	// must invalidate denyA, and consuming denyB must invalidate approveB
	// (so a user can't both approve and deny the same target). Cross-task
	// siblings remain independent because they were minted from separate
	// Generate calls — approveA / denyA carry task-A's entry, approveB /
	// denyB carry task-B's, and consuming any one of the four does NOT
	// touch the other task's pair.
	if _, err := s.Consume(denyA); err != errTokenUsed {
		t.Fatalf("Consume(denyA after approveA): want errTokenUsed, got %v", err)
	}
	if _, err := s.Consume(approveB); err != errTokenUsed {
		t.Fatalf("Consume(approveB after denyB): want errTokenUsed, got %v", err)
	}
}
