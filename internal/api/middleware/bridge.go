package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// BridgeFromContext retrieves the authenticated bridge (OpenClaw plugin
// install identity) from a request context. Delegates to the exported store
// helper so cloud/enterprise packages can also read bridge context.
func BridgeFromContext(ctx context.Context) *store.BridgeToken {
	return store.BridgeFromContext(ctx)
}

// RequireBridge validates a bridge bearer token and injects the bridge into
// the request context. Rejects agent tokens (cvis_ prefix) outright so a
// stolen or misused agent token cannot reach bridge-scoped endpoints.
func RequireBridge(st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				http.Error(w, `{"error":"missing authorization header","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}
			// Explicit prefix check so agent tokens get a clear, distinct
			// rejection rather than falling through to an opaque hash miss.
			if !strings.HasPrefix(token, "cvisbr_") {
				http.Error(w, `{"error":"bridge token required for this endpoint","code":"WRONG_TOKEN_TYPE"}`, http.StatusUnauthorized)
				return
			}

			hash := auth.HashToken(token)
			bt, err := st.GetBridgeTokenByHash(r.Context(), hash)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					http.Error(w, `{"error":"invalid bridge token","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				} else {
					http.Error(w, `{"error":"temporary service error, please retry","code":"SERVICE_UNAVAILABLE"}`, http.StatusServiceUnavailable)
				}
				return
			}
			if bt.RevokedAt != nil {
				http.Error(w, `{"error":"bridge token has been revoked","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			// Best-effort last-used bookkeeping; failure here should not block
			// the request — it's cosmetic (dashboard column).
			_ = st.TouchBridgeTokenLastUsed(r.Context(), bt.ID)

			ctx := store.WithBridge(r.Context(), bt)
			AddLogField(ctx, "bridge_id", bt.ID)
			AddLogField(ctx, "user_id", bt.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
