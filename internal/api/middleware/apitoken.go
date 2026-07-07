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

// ValidTokenScope reports whether s is one of the three mintable scopes
// (config-read, config-write, instance-admin). Unknown scopes are rejected
// at mint time (fail closed).
func ValidTokenScope(s string) bool {
	_, ok := scopeRank[s]
	return ok
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
//
// apiTokensEnabled reflects the instance-wide FeatureSet.APITokens gate
// (auth.disable_api_tokens). When false and the request carries a `cvat_`
// bearer, the middleware short-circuits 401 WITHOUT consulting the token
// table — so a leaked / pre-existing / DB-planted token is inert on EVERY
// route this gate protects (mint, /api/users*, shared vault, restrictions).
// A `cvat_` is not a JWT, so a rejected bearer never falls through to JWT
// parsing; a normal JWT request (no `cvat_` prefix) still works unchanged.
func RequireUserOrAPIToken(jwtSvc auth.TokenService, st store.Store, minScope string, apiTokensEnabled bool) func(http.Handler) http.Handler {
	requireUser := RequireUser(jwtSvc, st)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := apiTokenFromRequest(r)
			if tok == "" {
				requireUser(next).ServeHTTP(w, r)
				return
			}
			if !apiTokensEnabled {
				// API tokens are disabled instance-wide. Reject the presented
				// cvat_ bearer without a token-table lookup and do NOT fall
				// through to JWT parsing (a cvat_ is not a JWT).
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "api tokens are disabled")
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

// RequireAdminOrToken gates the instance-administrative surface (user
// management, shared-vault writes, token management, governance-disabling
// changes — the spec's trust split). It accepts EITHER:
//   - a `cvat_` API token satisfying ScopeInstanceAdmin (config-write /
//     config-read tokens get 403 INSUFFICIENT_SCOPE from the inner
//     RequireUserOrAPIToken), OR
//   - a user JWT whose role is admin (members get 403 FORBIDDEN).
//
// Authorization precedence: when a token authenticated the request the gate
// is the token's scope and the injected `_instance` user's role is NEVER
// consulted. Only on the JWT path is the role checked.
func RequireAdminOrToken(jwtSvc auth.TokenService, st store.Store, apiTokensEnabled bool) func(http.Handler) http.Handler {
	requireUserOrToken := RequireUserOrAPIToken(jwtSvc, st, ScopeInstanceAdmin, apiTokensEnabled)
	return func(next http.Handler) http.Handler {
		return requireUserOrToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if APITokenFromContext(r.Context()) != nil {
				next.ServeHTTP(w, r)
				return
			}
			if u := UserFromContext(r.Context()); u == nil || u.Role != store.RoleAdmin {
				writeAuthError(w, http.StatusForbidden, "FORBIDDEN", "admin role required")
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}

// RejectInstanceItemWriteByScopedToken blocks per-user vault item writes
// (POST/PUT/DELETE /api/vault/items…) when the request authenticated with a
// non-instance-admin API token. Such a token is injected as the `_instance`
// system user, so a write through the config-write item gate would plant or
// delete a fleet-wide shared entry — exactly the boundary the instance-admin
// `/api/vault/shared` surface exists to guard. Instance-admin tokens and JWT
// users (who own their own rows) pass through untouched. Must run AFTER the
// RequireUserOrAPIToken gate so the token and injected user are in context.
func RejectInstanceItemWriteByScopedToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if at := APITokenFromContext(r.Context()); at != nil && at.Scope != ScopeInstanceAdmin {
			if u := UserFromContext(r.Context()); u != nil && u.ID == store.InstanceUserID {
				writeAuthError(w, http.StatusForbidden, "FORBIDDEN",
					"shared vault entries must be written via /api/vault/shared with an instance-admin token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
