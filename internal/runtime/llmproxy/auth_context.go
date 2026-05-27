package llmproxy

import (
	"context"
	"net/http"
)

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

// CallerAuthSource* values describe which header carried the validated
// agent token, so audit/diagnostic logs can distinguish at a glance
// between "client used X-Clawvisor-Agent-Token (passthrough intent)"
// vs "client put the agent token in Authorization / x-api-key".
const (
	CallerAuthSourceClawvisorHeader = "x-clawvisor-agent-token"
	CallerAuthSourceAuthorization   = "authorization"
	CallerAuthSourceXAPIKey         = "x-api-key"
)

type callerAuthSourceContextKey struct{}

// WithCallerAuthSource records which inbound header carried the agent
// token. Handlers read it back via CallerAuthSource to surface in the
// audit + completion logs.
func WithCallerAuthSource(ctx context.Context, source string) context.Context {
	if source == "" {
		return ctx
	}
	return context.WithValue(ctx, callerAuthSourceContextKey{}, source)
}

// CallerAuthSource returns the header that carried the agent token for
// this request, or "" if the middleware didn't record one.
func CallerAuthSource(ctx context.Context) string {
	v, _ := ctx.Value(callerAuthSourceContextKey{}).(string)
	return v
}

// HasPassthroughBearer reports whether the inbound request carries a
// non-Clawvisor Bearer token that the forwarder would re-use as the
// upstream Authorization header in passthrough mode. Exposed so the
// LLM endpoint can distinguish "passthrough requested but no bearer
// arrived" from a plain vault miss when surfacing errors.
func HasPassthroughBearer(r *http.Request) bool {
	return passthroughBearerAuthorization(r) != ""
}
