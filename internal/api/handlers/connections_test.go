package handlers

import (
	"context"
	"log/slog"
	"testing"
	"time"

	sqlitestore "github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type testNotifier struct {
	decremented int
	updated     []string
}

func (n *testNotifier) SendApprovalRequest(context.Context, notify.ApprovalRequest) (string, error) {
	return "", nil
}

func (n *testNotifier) SendActivationRequest(context.Context, notify.ActivationRequest) error {
	return nil
}

func (n *testNotifier) SendTaskApprovalRequest(context.Context, notify.TaskApprovalRequest) (string, error) {
	return "", nil
}

func (n *testNotifier) SendScopeExpansionRequest(context.Context, notify.ScopeExpansionRequest) (string, error) {
	return "", nil
}

func (n *testNotifier) UpdateMessage(_ context.Context, _ string, _ string, text string) error {
	n.updated = append(n.updated, text)
	return nil
}

func (n *testNotifier) SendTestMessage(context.Context, string) error {
	return nil
}

func (n *testNotifier) SendConnectionRequest(context.Context, notify.ConnectionRequest) (string, error) {
	return "", nil
}

func (n *testNotifier) SendAlert(context.Context, string, string) error {
	return nil
}

func (n *testNotifier) DecrementPolling(string) {
	n.decremented++
}

func TestConnectionsHandlerApproveUpdatesNotificationState(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "owner@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	req := &store.ConnectionRequest{
		UserID:    user.ID,
		Name:      "Claude Code",
		Status:    "pending",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if err := st.CreateConnectionRequest(ctx, req); err != nil {
		t.Fatalf("CreateConnectionRequest: %v", err)
	}
	if err := st.SaveNotificationMessage(ctx, "connection", req.ID, "telegram", "msg-1"); err != nil {
		t.Fatalf("SaveNotificationMessage: %v", err)
	}

	notifier := &testNotifier{}
	h := NewConnectionsHandler(st, notifier, nil, slog.Default(), "http://example.com", false)

	agentID, err := h.ApproveByID(ctx, req.ID, user.ID)
	if err != nil {
		t.Fatalf("ApproveByID: %v", err)
	}
	if agentID == "" {
		t.Fatal("expected agent ID")
	}
	if notifier.decremented != 1 {
		t.Fatalf("expected polling decrement once, got %d", notifier.decremented)
	}
	if len(notifier.updated) != 1 || notifier.updated[0] != "✅ <b>Approved</b> — agent connected." {
		t.Fatalf("unexpected notification updates: %#v", notifier.updated)
	}
}

func TestConnectionsHandlerExpireUpdatesNotificationState(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "owner@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	req := &store.ConnectionRequest{
		UserID:    user.ID,
		Name:      "Claude Code",
		Status:    "pending",
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	if err := st.CreateConnectionRequest(ctx, req); err != nil {
		t.Fatalf("CreateConnectionRequest: %v", err)
	}
	if err := st.SaveNotificationMessage(ctx, "connection", req.ID, "telegram", "msg-1"); err != nil {
		t.Fatalf("SaveNotificationMessage: %v", err)
	}

	notifier := &testNotifier{}
	h := NewConnectionsHandler(st, notifier, nil, slog.Default(), "http://example.com", false)

	if err := h.expireByID(ctx, req.ID, user.ID); err != nil {
		t.Fatalf("expireByID: %v", err)
	}

	got, err := st.GetConnectionRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("GetConnectionRequest: %v", err)
	}
	if got.Status != "expired" {
		t.Fatalf("expected expired status, got %q", got.Status)
	}
	if notifier.decremented != 1 {
		t.Fatalf("expected polling decrement once, got %d", notifier.decremented)
	}
	if len(notifier.updated) != 1 || notifier.updated[0] != "⏰ <b>Expired</b> — connection request timed out." {
		t.Fatalf("unexpected notification updates: %#v", notifier.updated)
	}
}
