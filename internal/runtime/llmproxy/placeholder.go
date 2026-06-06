package llmproxy

import (
	"context"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/placeholder"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func ValidateRuntimePlaceholderAccess(ctx context.Context, st store.Store, ph *store.RuntimePlaceholder, userID, agentID string, now time.Time) (string, bool) {
	return placeholder.ValidateRuntimePlaceholderAccess(ctx, st, ph, userID, agentID, now)
}

func RuntimePlaceholderBoundHosts(ctx context.Context, st store.Store, ph *store.RuntimePlaceholder) ([]string, string) {
	return placeholder.RuntimePlaceholderBoundHosts(ctx, st, ph)
}
