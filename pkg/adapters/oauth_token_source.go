package adapters

import (
	"context"

	"golang.org/x/oauth2"
)

type oauthTokenSourceKey struct{}

// WithOAuthTokenSource binds gateway-managed refresh and persistence to every
// HTTP call made for an adapter execution, including long-running actions.
func WithOAuthTokenSource(ctx context.Context, source oauth2.TokenSource) context.Context {
	return context.WithValue(ctx, oauthTokenSourceKey{}, source)
}

// OAuthTokenSourceFromContext is nil for standalone adapter use, where existing
// adapter-local refresh behavior remains available.
func OAuthTokenSourceFromContext(ctx context.Context) oauth2.TokenSource {
	source, _ := ctx.Value(oauthTokenSourceKey{}).(oauth2.TokenSource)
	return source
}

// ManagedOAuthTokenSource prefers a gateway persistence-aware source when one
// was supplied, otherwise preserving the adapter's standalone token source.
func ManagedOAuthTokenSource(ctx context.Context, fallback oauth2.TokenSource) oauth2.TokenSource {
	if source := OAuthTokenSourceFromContext(ctx); source != nil {
		return source
	}
	return fallback
}
