package llmproxy

import "context"

// MaxContinuationDepth caps the number of synthetic tool_result rounds
// the proxy will inject for a single inbound harness request. One is
// enough for the common case (auto-approve a task, let the model emit
// its next tool_use) and avoids an auto-approval gate looping forever
// on a misbehaving model that keeps re-emitting
// POST /api/control/tasks shapes.
const MaxContinuationDepth = 1

type continuationDepthCtxKey struct{}

// WithContinuationDepth returns a context whose
// ContinuationDepthFromContext lookup yields the given depth. Used by
// the handler to mark the recursive continuation request so a second
// pass through Postprocess does not request another continuation.
func WithContinuationDepth(parent context.Context, depth int) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithValue(parent, continuationDepthCtxKey{}, depth)
}

// ContinuationDepthFromContext reports how many continuation rounds
// have already been performed for the current inbound harness request.
// Returns 0 when the context has no marker (i.e. this is the original
// upstream call).
func ContinuationDepthFromContext(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	v, _ := ctx.Value(continuationDepthCtxKey{}).(int)
	return v
}
