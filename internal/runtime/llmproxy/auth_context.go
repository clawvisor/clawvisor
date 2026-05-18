package llmproxy

import "context"

type upstreamAuthModeContextKey struct{}

const upstreamAuthModePassthrough = "passthrough"

// WithPassthroughUpstreamAuth marks a lite-proxy request as authenticated to
// Clawvisor out-of-band, allowing the upstream Authorization header to remain
// the user's provider OAuth/subscription credential.
func WithPassthroughUpstreamAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, upstreamAuthModeContextKey{}, upstreamAuthModePassthrough)
}

// PassthroughUpstreamAuth reports whether the request should preserve a
// non-Clawvisor upstream Authorization header instead of injecting a vault key.
func PassthroughUpstreamAuth(ctx context.Context) bool {
	v, _ := ctx.Value(upstreamAuthModeContextKey{}).(string)
	return v == upstreamAuthModePassthrough
}
