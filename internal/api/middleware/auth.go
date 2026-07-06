package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/pkg/auth"
	intauth "github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// writeAuthError writes a JSON error response consistent with handler writeError format.
func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}

type contextKey string

const (
	// UserContextKey is the context key for the authenticated user.
	UserContextKey contextKey = "user"
)

// UserFromContext retrieves the authenticated user from a request context.
func UserFromContext(ctx context.Context) *store.User {
	u, _ := ctx.Value(UserContextKey).(*store.User)
	return u
}

// RequireUser is middleware that validates a user JWT and injects the user into
// the request context. Returns 401 if the token is missing or invalid.
func RequireUser(jwtSvc auth.TokenService, st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing authorization header")
				return
			}

			claims, err := jwtSvc.ValidateToken(token)
			if err != nil {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or expired token")
				return
			}

			// Reject purpose-restricted tokens (setup, totp_verify, register) — they
			// are only accepted by the specific endpoints that check for them.
			if claims.Purpose != "" {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or expired token")
				return
			}

			user, err := st.GetUserByID(r.Context(), claims.UserID)
			if err != nil {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "user not found")
				return
			}

			ctx := context.WithValue(r.Context(), UserContextKey, user)
			AddLogField(ctx, "user_id", user.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAdmin composes RequireUser and then rejects any principal whose
// role is not admin with 403 FORBIDDEN. JWT-only: use RequireAdminOrToken
// when an instance-admin API token should also satisfy the gate.
func RequireAdmin(jwtSvc auth.TokenService, st store.Store) func(http.Handler) http.Handler {
	requireUser := RequireUser(jwtSvc, st)
	return func(next http.Handler) http.Handler {
		return requireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u := UserFromContext(r.Context()); u == nil || u.Role != store.RoleAdmin {
				writeAuthError(w, http.StatusForbidden, "FORBIDDEN", "admin role required")
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}

// OptionalUser is middleware that validates a user JWT if present and injects
// the user into the request context. Unlike RequireUser it never rejects the
// request: missing, invalid, or purpose-restricted tokens cause the handler
// to run without a user in context. Use this for routes that must be reachable
// pre-login but want to vary their response when a user is authenticated
// (e.g. /api/features for per-user feature gating).
func OptionalUser(jwtSvc auth.TokenService, st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				next.ServeHTTP(w, r)
				return
			}
			claims, err := jwtSvc.ValidateToken(token)
			if err != nil || claims.Purpose != "" {
				next.ServeHTTP(w, r)
				return
			}
			user, err := st.GetUserByID(r.Context(), claims.UserID)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			AddLogField(ctx, "user_id", user.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireUserOrTicket is middleware that first tries JWT auth (via Authorization
// header), then falls back to a single-use ticket query parameter. This is used
// for SSE endpoints where EventSource cannot set custom headers.
func RequireUserOrTicket(jwtSvc auth.TokenService, st store.Store, tickets intauth.TicketStorer) func(http.Handler) http.Handler {
	jwtMiddleware := RequireUser(jwtSvc, st)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Try JWT first.
			if bearerToken(r) != "" {
				jwtMiddleware(next).ServeHTTP(w, r)
				return
			}

			// Fall back to ticket query param.
			ticket := r.URL.Query().Get("ticket")
			if ticket == "" {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing authorization")
				return
			}

			userID, err := tickets.Validate(ticket)
			if err != nil {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or expired ticket")
				return
			}

			user, err := st.GetUserByID(r.Context(), userID)
			if err != nil {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "user not found")
				return
			}

			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireUserOrAgent accepts either a user JWT or a `cvis_…` agent token.
// When an agent token authenticates, the owning user is also resolved and
// attached to the request context, so handlers reading UserFromContext
// continue to work unchanged regardless of which auth shape the caller used.
//
// Used on /api/runtime/llm-credentials/* so the lite-proxy install skill —
// which holds the freshly-minted agent token but no dashboard session — can
// vault the user's upstream LLM API key during one-paste setup.
//
// Token-shape sniff: a value in `X-Clawvisor-Agent-Token`, or an
// `Authorization: Bearer cvis_…` value, is treated as an agent token.
// Anything else in the Authorization header is sent down the user-JWT path.
func RequireUserOrAgent(jwtSvc auth.TokenService, st store.Store) func(http.Handler) http.Handler {
	requireUser := RequireUser(jwtSvc, st)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := agentTokenFromRequest(r)
			if tok == "" {
				requireUser(next).ServeHTTP(w, r)
				return
			}
			hash := intauth.HashToken(tok)
			agent, err := st.GetAgentByToken(r.Context(), hash)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid agent token")
				} else {
					writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "temporary service error, please retry")
				}
				return
			}
			if agent.TokenExpiresAt != nil && time.Now().After(*agent.TokenExpiresAt) {
				writeAuthError(w, http.StatusUnauthorized, "TOKEN_EXPIRED", "agent token has expired")
				return
			}
			user, err := st.GetUserByID(r.Context(), agent.UserID)
			if err != nil {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "owning user not found")
				return
			}
			ctx := store.WithAgent(r.Context(), agent)
			ctx = context.WithValue(ctx, UserContextKey, user)
			AddLogField(ctx, "agent_id", agent.ID)
			AddLogField(ctx, "user_id", user.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// agentTokenFromRequest returns a `cvis_…` agent token sniffed from either the
// dedicated header or the Authorization bearer slot. Used by RequireUserOrAgent
// to decide whether to take the agent-auth branch before consulting the JWT.
func agentTokenFromRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Clawvisor-Agent-Token")); v != "" {
		const prefix = "Bearer "
		if strings.HasPrefix(v, prefix) {
			v = strings.TrimSpace(v[len(prefix):])
		}
		if strings.HasPrefix(v, "cvis_") {
			return v
		}
	}
	if bt := bearerToken(r); strings.HasPrefix(bt, "cvis_") {
		return bt
	}
	return ""
}

// bearerToken extracts the token value from "Authorization: Bearer <token>".
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
