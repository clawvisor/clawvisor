package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// stubChannel is a notifier that succeeds or fails its test message,
// standing in for a configured / unconfigured channel.
type stubChannel struct {
	Notifier
	name    string
	testErr error
	sent    *[]string
}

func (s stubChannel) SendTestMessage(_ context.Context, _ string) error {
	if s.testErr != nil {
		return s.testErr
	}
	*s.sent = append(*s.sent, s.name)
	return nil
}

type stubTelegram struct{ stubChannel }

func (s stubTelegram) SendTelegramTestMessage(ctx context.Context, u string) error {
	return s.SendTestMessage(ctx, u)
}

type stubSlack struct{ stubChannel }

func (s stubSlack) SendSlackTestMessage(ctx context.Context, u string) error {
	return s.SendTestMessage(ctx, u)
}

func testMulti(t *testing.T, sent *[]string) *MultiNotifier {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Telegram unconfigured (as in a Slack-only setup), Slack healthy.
	tg := stubTelegram{stubChannel{name: "telegram", testErr: errors.New("no bot token configured"), sent: sent}}
	sl := stubSlack{stubChannel{name: "slack", sent: sent}}
	return NewMultiNotifier(context.Background(), logger, tg, sl)
}

// The bug this pins: the Slack test button called Notifier.SendTestMessage,
// which fans out to every channel and joins the errors — so an unconfigured
// Telegram made a successfully-delivered Slack message report as failed.
func TestSendSlackTestMessage_IgnoresOtherChannelFailures(t *testing.T) {
	var sent []string
	m := testMulti(t, &sent)

	if err := m.SendSlackTestMessage(context.Background(), "user-1"); err != nil {
		t.Fatalf("Slack test reported failure despite Slack succeeding: %v", err)
	}
	if len(sent) != 1 || sent[0] != "slack" {
		t.Fatalf("expected only Slack to be sent, got %v", sent)
	}
}

// The symmetric regression: adding Slack to the notifier chain must not make
// a working Telegram test report failure when Slack is unconfigured.
func TestSendTelegramTestMessage_IgnoresOtherChannelFailures(t *testing.T) {
	var sent []string
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tg := stubTelegram{stubChannel{name: "telegram", sent: &sent}}
	sl := stubSlack{stubChannel{name: "slack", testErr: errors.New("no bot token configured"), sent: &sent}}
	m := NewMultiNotifier(context.Background(), logger, tg, sl)

	if err := m.SendTelegramTestMessage(context.Background(), "user-1"); err != nil {
		t.Fatalf("Telegram test reported failure despite Telegram succeeding: %v", err)
	}
	if len(sent) != 1 || sent[0] != "telegram" {
		t.Fatalf("expected only Telegram to be sent, got %v", sent)
	}
}

// The fan-out method keeps its broadcast semantics — it is still what a
// channel-agnostic caller wants.
func TestSendTestMessage_StillFansOutAndReportsErrors(t *testing.T) {
	var sent []string
	m := testMulti(t, &sent)

	if err := m.SendTestMessage(context.Background(), "user-1"); err == nil {
		t.Fatal("fan-out send should surface the failing channel's error")
	}
	if len(sent) != 1 || sent[0] != "slack" {
		t.Fatalf("healthy channel should still receive the message, got %v", sent)
	}
}

// A deployment without Slack must report it as unavailable rather than
// silently succeeding.
func TestSendSlackTestMessage_ErrorsWhenSlackAbsent(t *testing.T) {
	var sent []string
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tg := stubTelegram{stubChannel{name: "telegram", sent: &sent}}
	m := NewMultiNotifier(context.Background(), logger, tg)

	if err := m.SendSlackTestMessage(context.Background(), "user-1"); err == nil {
		t.Fatal("expected an error when no Slack notifier is configured")
	}
	if len(sent) != 0 {
		t.Fatalf("no message should have been sent, got %v", sent)
	}
}
