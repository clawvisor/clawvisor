package handlers

import (
	"context"
	"errors"
	"log/slog"

	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// notificationMessageLookup is the slice of store.Store that
// updateNotificationMessage needs. Narrowing it keeps the helper testable
// without standing up a full store.
type notificationMessageLookup interface {
	GetNotificationMessage(ctx context.Context, targetType, targetID, channel string) (string, error)
}

// updateNotificationMessage rewrites the already-delivered notification for an
// approval target across every configured channel. Approvals, connections and
// tasks all resolve the same way, so this lives in one place: a new editing
// channel is then wired once rather than in each handler.
//
// Both updates are attempted independently and failures are only logged —
// a resolve must not fail because a chat message could not be edited.
func updateNotificationMessage(
	ctx context.Context,
	st notificationMessageLookup,
	notifier notify.Notifier,
	logger *slog.Logger,
	targetType, targetID, userID, text string,
) {
	// This helper exists to be called on paths that must not fail, including
	// from test harnesses that supply no logger. Before entry logging was
	// added the first statement was the nil-notifier guard, so a nil logger
	// was never dereferenced; it is now, so it has to be tolerated.
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	// Entry is logged because every exit from here is a deliberate no-op.
	// With only the failure paths instrumented, "never called" and "called
	// and did nothing" produce identical logs — which is what made a prompt
	// keeping its buttons after approval impossible to place.
	logger.InfoContext(ctx, "resolving notification message",
		"target_type", targetType, "target_id", targetID)

	if notifier == nil {
		logger.WarnContext(ctx, "no notifier configured, cannot update prompt",
			"target_type", targetType, "target_id", targetID)
		return
	}
	msgID, err := st.GetNotificationMessage(ctx, targetType, targetID, "telegram")
	switch {
	case err == nil:
		if err := notifier.UpdateMessage(ctx, userID, msgID, text); err != nil {
			logger.WarnContext(ctx, "telegram message update failed", "err", err, "target_type", targetType, "target_id", targetID)
		}
	case errors.Is(err, store.ErrNotFound):
		// No Telegram message for this target — nothing to edit. Expected
		// whenever Telegram is not a configured channel.
	default:
		// A storage failure is not the same as "nothing to edit": staying
		// silent here leaves a resolved request still showing live buttons
		// with no trace of why, which during a database outage is exactly
		// when someone would be trying to work out what happened.
		logger.WarnContext(ctx, "notification message lookup failed", "err", err, "target_type", targetType, "target_id", targetID)
	}
	// Channels that address messages by approval target rather than by an
	// opaque message ID (Slack) resolve their own reference — the ID above
	// is Telegram's and means nothing to them. This must stay outside the
	// lookup above: a missing Telegram message must not skip the
	// target-addressed update. No-op when no such channel is configured.
	tu, ok := notifier.(notify.TargetMessageUpdater)
	if !ok {
		// The notifier chain carries no channel that addresses messages by
		// target. Expected with Telegram alone; a bug if Slack is enabled.
		logger.InfoContext(ctx, "notifier has no target-addressed channel",
			"target_type", targetType, "target_id", targetID)
	}
	if ok {
		if err := tu.UpdateMessageForTarget(ctx, userID, targetType, targetID, text); err != nil {
			logger.WarnContext(ctx, "target-addressed message update failed", "err", err, "target_type", targetType, "target_id", targetID)
		}
	}
}
