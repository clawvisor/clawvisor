package llmproxy

import (
	"context"
)

// BuildScopeDriftContinuation registers a new drift record from the
// supplied template and returns the menu text the agent should see plus
// the minted drift_id. Callers thread driftID into audit/trace lines so
// operators can correlate the block with the registry record.
//
// On registry failure (nil registry, Register error), returns ("", "",
// err) so the caller can fall back to the legacy approval prompt
// without leaving a half-minted drift behind.
//
// The renderer reads controlBaseURL to build the (a)/(b) endpoint URLs
// the agent posts to. Empty controlBaseURL renders path-only routes —
// the caller is expected to supply the proxy's RoutingContext.ControlBaseURL.
func BuildScopeDriftContinuation(
	ctx context.Context,
	registry ScopeDriftRegistry,
	template ScopeDrift,
	controlBaseURL string,
) (menuText, driftID string, err error) {
	if registry == nil {
		return "", "", ErrDriftNotFound
	}
	drift, err := registry.Register(ctx, template)
	if err != nil {
		return "", "", err
	}
	return renderScopeDriftMenu(drift.MenuFields(), controlBaseURL), drift.ID, nil
}
