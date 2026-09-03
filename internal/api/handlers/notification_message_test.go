package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

// stubUpdateStore returns a fixed answer for the Telegram message lookup.
type stubUpdateStore struct {
	msgID string
	err   error
	calls int
}

func (s *stubUpdateStore) GetNotificationMessage(context.Context, string, string, string) (string, error) {
	s.calls++
	return s.msgID, s.err
}

// stubNotifier records the two update paths. Only UpdateMessage is exercised;
// the rest of notify.Notifier exists to satisfy the interface.
type stubNotifier struct {
	updated       []string
	targetUpdated []string
	updateErr     error
	targetErr     error
}

func (n *stubNotifier) SendApprovalRequest(context.Context, notify.ApprovalRequest) (string, error) {
	return "", nil
}
func (n *stubNotifier) SendActivationRequest(context.Context, notify.ActivationRequest) error {
	return nil
}
func (n *stubNotifier) SendTaskApprovalRequest(context.Context, notify.TaskApprovalRequest) (string, error) {
	return "", nil
}
func (n *stubNotifier) SendScopeExpansionRequest(context.Context, notify.ScopeExpansionRequest) (string, error) {
	return "", nil
}
func (n *stubNotifier) UpdateMessage(_ context.Context, _, messageID, _ string) error {
	n.updated = append(n.updated, messageID)
	return n.updateErr
}
func (n *stubNotifier) SendTestMessage(context.Context, string) error { return nil }
func (n *stubNotifier) SendConnectionRequest(context.Context, notify.ConnectionRequest) (string, error) {
	return "", nil
}
func (n *stubNotifier) SendAlert(context.Context, string, string) error { return nil }

// targetNotifier adds the Slack-style target-addressed update.
type targetNotifier struct{ stubNotifier }

func (n *targetNotifier) UpdateMessageForTarget(_ context.Context, _, targetType, targetID, _ string) error {
	n.targetUpdated = append(n.targetUpdated, targetType+"/"+targetID)
	return n.targetErr
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A user who only ever wired Slack has no Telegram message row, and an
// earlier version nested the target-addressed update inside the Telegram
// lookup — so the Slack prompt kept its live buttons after the approval was
// resolved. The two updates must stay independent.
func TestUpdateNotificationMessage_MissingTelegramRowStillUpdatesTarget(t *testing.T) {
	st := &stubUpdateStore{err: errors.New("no such row")}
	n := &targetNotifier{}

	updateNotificationMessage(context.Background(), st, n, testLogger(), "approval", "req-1", "user-1", "resolved")

	if len(n.updated) != 0 {
		t.Fatalf("telegram update ran without a message row: %v", n.updated)
	}
	if want := []string{"approval/req-1"}; len(n.targetUpdated) != 1 || n.targetUpdated[0] != want[0] {
		t.Fatalf("target update = %v, want %v", n.targetUpdated, want)
	}
}

// Both channels get the edit when both are reachable, and a failure on the
// Telegram side must not skip the target-addressed one.
func TestUpdateNotificationMessage_UpdatesBothChannels(t *testing.T) {
	st := &stubUpdateStore{msgID: "42"}
	n := &targetNotifier{stubNotifier: stubNotifier{updateErr: errors.New("telegram down")}}

	updateNotificationMessage(context.Background(), st, n, testLogger(), "task", "task-9", "user-1", "resolved")

	if len(n.updated) != 1 || n.updated[0] != "42" {
		t.Fatalf("telegram update = %v, want [42]", n.updated)
	}
	if len(n.targetUpdated) != 1 || n.targetUpdated[0] != "task/task-9" {
		t.Fatalf("target update = %v, want [task/task-9]", n.targetUpdated)
	}
}

// A deployment with only Telegram has no TargetMessageUpdater; the helper
// must degrade to the message-ID path rather than requiring the interface.
func TestUpdateNotificationMessage_NoTargetUpdater(t *testing.T) {
	st := &stubUpdateStore{msgID: "7"}
	n := &stubNotifier{}

	updateNotificationMessage(context.Background(), st, n, testLogger(), "connection", "conn-3", "user-1", "resolved")

	if len(n.updated) != 1 || n.updated[0] != "7" {
		t.Fatalf("telegram update = %v, want [7]", n.updated)
	}
}

// Notifications are optional, so a nil notifier must be a silent no-op that
// does not even hit the store.
func TestUpdateNotificationMessage_NilNotifier(t *testing.T) {
	st := &stubUpdateStore{msgID: "1"}

	updateNotificationMessage(context.Background(), st, nil, testLogger(), "approval", "req-1", "user-1", "resolved")

	if st.calls != 0 {
		t.Fatalf("store consulted %d times with a nil notifier", st.calls)
	}
}
