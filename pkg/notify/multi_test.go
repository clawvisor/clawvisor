package notify

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

type recordingNotifier struct {
	channel       string
	approvalIDs   []string
	updatedIDs    []string
	activationHit int
}

func (n *recordingNotifier) NotificationChannel() string { return n.channel }

func (n *recordingNotifier) SendApprovalRequest(ctx context.Context, req ApprovalRequest) (string, error) {
	n.approvalIDs = append(n.approvalIDs, req.RequestID)
	return n.channel + "-message", nil
}

func (n *recordingNotifier) SendActivationRequest(ctx context.Context, req ActivationRequest) error {
	n.activationHit++
	return nil
}

func (n *recordingNotifier) SendTaskApprovalRequest(ctx context.Context, req TaskApprovalRequest) (string, error) {
	return "", nil
}

func (n *recordingNotifier) SendScopeExpansionRequest(ctx context.Context, req ScopeExpansionRequest) (string, error) {
	return "", nil
}

func (n *recordingNotifier) UpdateMessage(ctx context.Context, userID, messageID, text string) error {
	n.updatedIDs = append(n.updatedIDs, messageID)
	return nil
}

func (n *recordingNotifier) SendTestMessage(ctx context.Context, userID string) error { return nil }

func (n *recordingNotifier) SendConnectionRequest(ctx context.Context, req ConnectionRequest) (string, error) {
	return "", nil
}

func (n *recordingNotifier) SendAlert(ctx context.Context, userID, text string) error { return nil }

func TestMultiNotifierSendApprovalRequestToChannel(t *testing.T) {
	ctx := context.Background()
	push := &recordingNotifier{channel: "push"}
	telegram := &recordingNotifier{channel: "telegram"}
	m := NewMultiNotifier(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), push, telegram)

	msgID, err := m.SendApprovalRequestToChannel(ctx, "telegram", ApprovalRequest{RequestID: "req-1"})
	if err != nil {
		t.Fatalf("SendApprovalRequestToChannel: %v", err)
	}
	if msgID != "telegram-message" {
		t.Fatalf("messageID=%q, want telegram-message", msgID)
	}
	if len(push.approvalIDs) != 0 {
		t.Fatalf("push approvals=%d, want 0", len(push.approvalIDs))
	}
	if len(telegram.approvalIDs) != 1 {
		t.Fatalf("telegram approvals=%d, want 1", len(telegram.approvalIDs))
	}
}

func TestMultiNotifierSendApprovalRequestStillFansOut(t *testing.T) {
	ctx := context.Background()
	push := &recordingNotifier{channel: "push"}
	telegram := &recordingNotifier{channel: "telegram"}
	m := NewMultiNotifier(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), push, telegram)

	if _, err := m.SendApprovalRequest(ctx, ApprovalRequest{RequestID: "req-1"}); err != nil {
		t.Fatalf("SendApprovalRequest: %v", err)
	}
	if len(push.approvalIDs) != 1 || len(telegram.approvalIDs) != 1 {
		t.Fatalf("fanout approvals push=%d telegram=%d, want 1/1", len(push.approvalIDs), len(telegram.approvalIDs))
	}
}

func TestMultiNotifierUnknownChannelReturnsControlledError(t *testing.T) {
	ctx := context.Background()
	m := NewMultiNotifier(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), &recordingNotifier{channel: "push"})

	if _, err := m.SendApprovalRequestToChannel(ctx, "sms", ApprovalRequest{RequestID: "req-1"}); err == nil {
		t.Fatal("expected unavailable channel error")
	}
}

func TestMultiNotifierUpdateMessageForChannel(t *testing.T) {
	ctx := context.Background()
	push := &recordingNotifier{channel: "push"}
	telegram := &recordingNotifier{channel: "telegram"}
	m := NewMultiNotifier(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), push, telegram)

	if err := m.UpdateMessageForChannel(ctx, "telegram", "user-1", "msg-1", "done"); err != nil {
		t.Fatalf("UpdateMessageForChannel: %v", err)
	}
	if len(push.updatedIDs) != 0 {
		t.Fatalf("push updates=%d, want 0", len(push.updatedIDs))
	}
	if len(telegram.updatedIDs) != 1 || telegram.updatedIDs[0] != "msg-1" {
		t.Fatalf("telegram updates=%v, want [msg-1]", telegram.updatedIDs)
	}
}
