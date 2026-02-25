package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/store"
)

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
func RequireUser(jwtSvc *auth.JWTService, st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				http.Error(w, `{"error":"missing authorization header","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			claims, err := jwtSvc.ValidateToken(token)
			if err != nil {
				http.Error(w, `{"error":"invalid or expired token","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			user, err := st.GetUserByID(r.Context(), claims.UserID)
			if err != nil {
				http.Error(w, `{"error":"user not found","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
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
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
