package handlers

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/notify/slack"
	"github.com/clawvisor/clawvisor/pkg/notify"
)

// The sentinel must stay a single value shared with the notifier: the API
// hides it and the send path refuses it, and a second literal would let those
// two disagree about what "pending" spells.
func TestSlackPendingChannelMatchesNotifier(t *testing.T) {
	if slackPendingChannel != slack.PendingChannel {
		t.Fatalf("handler sentinel %q != notifier sentinel %q",
			slackPendingChannel, slack.PendingChannel)
	}
}

// A workspace waiting on a channel choice is connected, but must not report a
// channel the UI would render as configured.
func TestSlackConfigView_HidesPendingChannel(t *testing.T) {
	got := slackConfigView(notify.SlackConfig{
		TeamID: "T1", ChannelID: slackPendingChannel, ChannelName: "ignored",
	})
	if !got.Connected {
		t.Fatal("pending install reported as not connected")
	}
	if got.ChannelID != "" || got.ChannelName != "" {
		t.Fatalf("pending sentinel leaked to the API as %q/%q", got.ChannelID, got.ChannelName)
	}
}
