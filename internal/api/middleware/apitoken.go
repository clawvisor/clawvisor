package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	intauth "github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// API-token scopes. 05-lite issues and accepts only ScopeInstanceAdmin;
// the config-write / config-read split (and the trust split that gates
// governance-loosening and shared-vault writes) lands with spec 04. The
// constants and ranking are defined now so 04 extends without churn and
// so the provider resource (06b) has a stable vocabulary. Higher rank
// implies lower (hierarchical).
const (
	ScopeConfigRead    = "config-read"
	ScopeConfigWrite   = "config-write"
	ScopeInstanceAdmin = "instance-admin"
)

// scopeRank maps a scope to its authority level. Unknown scopes rank 0
// (satisfy nothing) — fail closed.
var scopeRank = map[string]int{
	ScopeConfigRead:    1,
	ScopeConfigWrite:   2,
	ScopeInstanceAdmin: 3,
}

// ScopeSatisfies reports whether a token carrying tokenScope meets the
// minScope requirement under the hierarchy (instance-admin > config-write
// > config-read).
func ScopeSatisfies(tokenScope, minScope string) bool {
	return scopeRank[tokenScope] >= scopeRank[minScope] && scopeRank[minScope] > 0
}

// apiTokenContextKey is the context key for the authenticated API token.
type apiTokenContextKey struct{}

// APITokenFromContext returns the API token that authenticated the
// request, or nil for JWT/agent auth. Privileged handlers check this
// FIRST: when a token is present the gate is the token's scope, and the
// injected `_instance` user's role is never consulted (see the spec's
// authorization-precedence rule).
func APITokenFromContext(ctx context.Context) *store.APIToken {
	t, _ := ctx.Value(apiTokenContextKey{}).(*store.APIToken)
	return t
}

func withAPIToken(ctx context.Context, t *store.APIToken) context.Context {
	return context.WithValue(ctx, apiTokenContextKey{}, t)
}

// lastUsedInterval is the throttle window for last_used_at writes.
const lastUsedInterval = time.Minute

// lastUsedThrottle rate-limits last_used_at writes to once per minute per
// token so a busy CI credential doesn't turn every request into a write.
type lastUsedThrottle struct {
	mu        sync.Mutex
	seen      map[string]time.Time
	lastSweep time.Time
}

func (l *lastUsedThrottle) shouldTouch(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// Evict entries older than the throttle window so the map can't grow
	// without bound as short-lived tokens are minted and revoked over time.
	// An expired entry is meaningless (it would be treated as touchable and
	// overwritten anyway), so dropping it changes no behavior. Sweep at most
	// once per window to keep this O(1) amortized.
	if now.Sub(l.lastSweep) >= lastUsedInterval {
		for k, ts := range l.seen {
			if now.Sub(ts) >= lastUsedInterval {
				delete(l.seen, k)
			}
		}
		l.lastSweep = now
	}
	if last, ok := l.seen[id]; ok && now.Sub(last) < lastUsedInterval {
		return false
	}
	l.seen[id] = now
	return true
}

var apiTokenLastUsed = &lastUsedThrottle{seen: map[string]time.Time{}}

// RequireUserOrAPIToken accepts either a user JWT or a `cvat_…` API token.
// When a `cvat_` value is present in the bearer slot it is validated as an
// API token: hashed, looked up, checked for revocation / expiry / scope,
// and on success the `_instance` system user is injected under
// UserContextKey (so every user-scoped handler owns rows under `_instance`)
// plus the token record under the API-token context key. The acting
// principal for logs is the token (token_id / token_name), NOT `_instance`.
//
// No `cvat_` prefix → fall through to RequireUser exactly as
// RequireUserOrAgent falls through to the JWT path today. The `cvat_`
// sniff happens BEFORE any JWT parsing so the shared bearer slot never
// hands an API token to the JWT validator.
func RequireUserOrAPIToken(jwtSvc auth.TokenService, st store.Store, minScope string) func(http.Handler) http.Handler {
	requireUser := RequireUser(jwtSvc, st)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := apiTokenFromRequest(r)
			if tok == "" {
				requireUser(next).ServeHTTP(w, r)
				return
			}

			at, err := st.GetAPITokenByHash(r.Context(), intauth.HashToken(tok))
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid api token")
				} else {
					writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "temporary service error, please retry")
				}
				return
			}
			if at.RevokedAt != nil {
				writeAuthError(w, http.StatusUnauthorized, "TOKEN_REVOKED", "api token has been revoked")
				return
			}
			if at.ExpiresAt != nil && time.Now().After(*at.ExpiresAt) {
				writeAuthError(w, http.StatusUnauthorized, "TOKEN_EXPIRED", "api token has expired")
				return
			}
			if !ScopeSatisfies(at.Scope, minScope) {
				writeAuthError(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "api token scope is insufficient for this operation")
				return
			}

			// Attribution: token-authenticated writes are owned by the
			// `_instance` system user. Fail CLOSED (500) if that row is
			// missing rather than silently attributing to a random user.
			user, err := st.GetUserByID(r.Context(), store.InstanceUserID)
			if err != nil {
				writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "instance system user is not provisioned")
				return
			}

			if apiTokenLastUsed.shouldTouch(at.ID) {
				// Best-effort; a failed touch must not fail the request.
				_ = st.TouchAPITokenLastUsed(r.Context(), at.ID)
			}

			ctx := withAPIToken(r.Context(), at)
			ctx = context.WithValue(ctx, UserContextKey, user)
			// Principal is the token, not `_instance`: this keeps each
			// CI/automation token individually attributable and bounds a
			// revocation's blast radius to one named token.
			AddLogField(ctx, "token_id", at.ID)
			AddLogField(ctx, "token_name", at.Name)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// apiTokenFromRequest returns a `cvat_…` API token sniffed from the
// Authorization bearer slot, or "" if the bearer is absent or carries a
// different shape (JWT, cvis_ agent token). Mirrors agentTokenFromRequest.
func apiTokenFromRequest(r *http.Request) string {
	if bt := bearerToken(r); strings.HasPrefix(bt, intauth.APITokenPrefix) {
		return bt
	}
	return ""
}
