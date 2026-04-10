package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

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

// RequireUserOrTicket is middleware that first tries JWT auth (via Authorization
// header), then falls back to a single-use ticket query parameter. This is used
// for SSE endpoints where EventSource cannot set custom headers.
func RequireUserOrTicket(jwtSvc auth.TokenService, st store.Store, tickets *intauth.TicketStore) func(http.Handler) http.Handler {
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

// bearerToken extracts the token value from "Authorization: Bearer <token>".
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
