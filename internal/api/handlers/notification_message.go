package handlers

import (
	"context"
	"log/slog"

	"github.com/clawvisor/clawvisor/pkg/notify"
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
	if notifier == nil {
		return
	}
	if msgID, err := st.GetNotificationMessage(ctx, targetType, targetID, "telegram"); err == nil {
		if err := notifier.UpdateMessage(ctx, userID, msgID, text); err != nil {
			logger.WarnContext(ctx, "telegram message update failed", "err", err, "target_type", targetType, "target_id", targetID)
		}
	}
	// Channels that address messages by approval target rather than by an
	// opaque message ID (Slack) resolve their own reference — the ID above
	// is Telegram's and means nothing to them. This must stay outside the
	// lookup above: a missing Telegram message must not skip the
	// target-addressed update. No-op when no such channel is configured.
	if tu, ok := notifier.(notify.TargetMessageUpdater); ok {
		if err := tu.UpdateMessageForTarget(ctx, userID, targetType, targetID, text); err != nil {
			logger.WarnContext(ctx, "target-addressed message update failed", "err", err, "target_type", targetType, "target_id", targetID)
		}
	}
}
