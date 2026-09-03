package notify

import (
	"context"
	"testing"
)

type recordingNotifier struct {
	channel string
	id      string
	updates []string
}

func (n *recordingNotifier) NotificationChannel() string { return n.channel }
func (n *recordingNotifier) SendApprovalRequest(context.Context, ApprovalRequest) (string, error) {
	return n.id, nil
}
func (n *recordingNotifier) SendActivationRequest(context.Context, ActivationRequest) error { return nil }
func (n *recordingNotifier) SendTaskApprovalRequest(context.Context, TaskApprovalRequest) (string, error) {
	return n.id, nil
}
func (n *recordingNotifier) SendScopeExpansionRequest(context.Context, ScopeExpansionRequest) (string, error) {
	return n.id, nil
}
func (n *recordingNotifier) UpdateMessage(_ context.Context, _ string, messageID, _ string) error {
	n.updates = append(n.updates, messageID)
	return nil
}
func (n *recordingNotifier) SendTestMessage(context.Context, string) error { return nil }
func (n *recordingNotifier) SendConnectionRequest(context.Context, ConnectionRequest) (string, error) {
	return n.id, nil
}
func (n *recordingNotifier) SendAlert(context.Context, string, string) error { return nil }

func TestMultiNotifierRoutesEncodedMessageUpdatesByChannel(t *testing.T) {
	ctx := context.Background()
	tg := &recordingNotifier{channel: "telegram", id: "42"}
	sl := &recordingNotifier{channel: "slack", id: "C123:1700000000.000100"}
	m := NewMultiNotifier(ctx, nil, tg, sl)

	messageID, err := m.SendApprovalRequest(ctx, ApprovalRequest{})
	if err != nil {
		t.Fatalf("SendApprovalRequest: %v", err)
	}
	if messageID == "" || messageID == "42" || messageID == "C123:1700000000.000100" {
		t.Fatalf("expected encoded multi-channel message id, got %q", messageID)
	}
	if err := m.UpdateMessage(ctx, "u1", messageID, "approved"); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	if len(tg.updates) != 1 || tg.updates[0] != "42" {
		t.Fatalf("telegram updates = %#v", tg.updates)
	}
	if len(sl.updates) != 1 || sl.updates[0] != "C123:1700000000.000100" {
		t.Fatalf("slack updates = %#v", sl.updates)
	}
}
