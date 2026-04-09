package store

import "context"

// AgentContextKey is the context key type for the authenticated agent.
// Exported so that cloud/enterprise packages can read agent context values
// set by the internal RequireAgent middleware.
type AgentContextKey struct{}

// AgentFromContext retrieves the authenticated agent from a request context.
// Returns nil if no agent is set.
func AgentFromContext(ctx context.Context) *Agent {
	a, _ := ctx.Value(AgentContextKey{}).(*Agent)
	return a
}

// WithAgent returns a new context with the given agent set.
func WithAgent(ctx context.Context, agent *Agent) context.Context {
	return context.WithValue(ctx, AgentContextKey{}, agent)
}
