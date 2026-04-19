package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ProxyFromContext retrieves the authenticated Clawvisor Proxy instance
// from a request context. Delegates to the exported store helper.
func ProxyFromContext(ctx context.Context) *store.ProxyInstance {
	return store.ProxyFromContext(ctx)
}

// RequireProxy validates a proxy bearer token (cvisproxy_...) and injects
// the proxy instance into the request context. Rejects any other token
// type so a stolen bridge or agent token can't reach proxy-scoped
// endpoints. See docs/proxy-api.md §4.2.
func RequireProxy(st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				http.Error(w, `{"error":"missing authorization header","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}
			if !strings.HasPrefix(token, "cvisproxy_") {
				http.Error(w, `{"error":"proxy token required for this endpoint","code":"WRONG_TOKEN_TYPE"}`, http.StatusUnauthorized)
				return
			}

			hash := auth.HashToken(token)
			p, err := st.GetProxyInstanceByHash(r.Context(), hash)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					http.Error(w, `{"error":"invalid proxy token","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				} else {
					http.Error(w, `{"error":"temporary service error, please retry","code":"SERVICE_UNAVAILABLE"}`, http.StatusServiceUnavailable)
				}
				return
			}
			if p.RevokedAt != nil {
				http.Error(w, `{"error":"proxy has been revoked","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			// Best-effort last-seen bookkeeping; failure doesn't block the request.
			_ = st.TouchProxyInstanceLastSeen(r.Context(), p.ID)

			ctx := store.WithProxy(r.Context(), p)
			AddLogField(ctx, "proxy_instance_id", p.ID)
			AddLogField(ctx, "bridge_id", p.BridgeID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
