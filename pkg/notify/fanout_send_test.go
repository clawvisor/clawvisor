package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// sendStub is a notifier whose send either fails or returns a message ID.
type sendStub struct {
	Notifier
	id  string
	err error
}

func (s sendStub) SendTaskApprovalRequest(context.Context, TaskApprovalRequest) (string, error) {
	return s.id, s.err
}

// telegramStub implements TelegramTester, which is how the fan-out
// identifies the channel whose message ID callers persist.
type telegramStub struct{ sendStub }

func (telegramStub) SendTelegramTestMessage(context.Context, string) error { return nil }

// selfRecordingStub also implements TargetMessageUpdater, marking it as a
// channel that stores its own message reference (Slack).
type selfRecordingStub struct{ sendStub }

func (selfRecordingStub) UpdateMessageForTarget(context.Context, string, string, string, string) error {
	return nil
}

func multi(t *testing.T, ns ...Notifier) *MultiNotifier {
	t.Helper()
	return NewMultiNotifier(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), ns...)
}

// The reported bug: with only Slack configured, the unconfigured Telegram
// notifier errored on every send, so callers logged "failed to send" for a
// prompt that had actually been delivered — and skipped the follow-up work
// they do on success.
func TestFanOutSend_PartialSuccessIsNotAFailure(t *testing.T) {
	tg := sendStub{err: errors.New("telegram: no notification configured")}
	slack := selfRecordingStub{sendStub{id: "C1:1700000000.1"}}

	_, err := multi(t, tg, slack).SendTaskApprovalRequest(context.Background(), TaskApprovalRequest{})
	if err != nil {
		t.Fatalf("a delivered prompt reported failure: %v", err)
	}
}

// Callers persist the returned ID against a hardcoded "telegram" channel, so
// a self-recording channel's reference must never be handed back — it would
// be written into Telegram's row.
func TestFanOutSend_DoesNotLeakSelfRecordingMessageID(t *testing.T) {
	tg := sendStub{err: errors.New("telegram: no notification configured")}
	slack := selfRecordingStub{sendStub{id: "C1:1700000000.1"}}

	id, err := multi(t, tg, slack).SendTaskApprovalRequest(context.Background(), TaskApprovalRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Fatalf("returned %q from a self-recording channel; it would be stored as a Telegram message ID", id)
	}
}

// When Telegram succeeds its ID is still what callers get.
func TestFanOutSend_ReturnsTelegramMessageID(t *testing.T) {
	tg := telegramStub{sendStub{id: "12345"}}
	slack := selfRecordingStub{sendStub{id: "C1:1700000000.1"}}

	id, err := multi(t, tg, slack).SendTaskApprovalRequest(context.Background(), TaskApprovalRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if id != "12345" {
		t.Fatalf("id = %q, want Telegram's %q", id, "12345")
	}
}

// If nothing reached the user, that is a real failure and must surface.
func TestFanOutSend_AllFailingSurfacesError(t *testing.T) {
	a := sendStub{err: errors.New("telegram down")}
	b := selfRecordingStub{sendStub{err: errors.New("slack down")}}

	id, err := multi(t, a, b).SendTaskApprovalRequest(context.Background(), TaskApprovalRequest{})
	if err == nil {
		t.Fatal("no channel delivered, but the send reported success")
	}
	if id != "" {
		t.Fatalf("id = %q, want empty when nothing was delivered", id)
	}
}

// Push returns ("", nil) when the user has no paired devices. Counting that
// as delivery let a genuine Telegram failure report success, so nobody
// learned the prompt never arrived.
func TestFanOutSend_NoOpDeliveryIsNotSuccess(t *testing.T) {
	tg := sendStub{err: errors.New("telegram: no notification configured")}
	push := sendStub{id: "", err: nil} // succeeded, reached nobody

	id, err := multi(t, tg, push).SendTaskApprovalRequest(context.Background(), TaskApprovalRequest{})
	if err == nil {
		t.Fatal("nothing reached the user, but the send reported success")
	}
	if id != "" {
		t.Fatalf("id = %q, want empty", id)
	}
}

// A self-recording channel proves delivery without returning an ID to the
// caller, so it must still count.
func TestFanOutSend_SelfRecordingCountsAsDelivery(t *testing.T) {
	tg := sendStub{err: errors.New("telegram: no notification configured")}
	slack := selfRecordingStub{sendStub{id: "C1:1700000000.1"}}

	if _, err := multi(t, tg, slack).SendTaskApprovalRequest(context.Background(), TaskApprovalRequest{}); err != nil {
		t.Fatalf("Slack delivered but the send reported failure: %v", err)
	}
}

// Push succeeds with its own reference, but callers persist whatever comes
// back under "telegram" — so returning push's would point later Telegram
// edits at a message that never existed.
func TestFanOutSend_DoesNotReturnPushMessageID(t *testing.T) {
	tg := telegramStub{sendStub{err: errors.New("telegram: no notification configured")}}
	push := sendStub{id: "push:daemon-1"}

	id, err := multi(t, tg, push).SendTaskApprovalRequest(context.Background(), TaskApprovalRequest{})
	if err != nil {
		t.Fatalf("push delivered, so the send should not report failure: %v", err)
	}
	if id != "" {
		t.Fatalf("id = %q; push's reference would be stored as a Telegram message ID", id)
	}
}
