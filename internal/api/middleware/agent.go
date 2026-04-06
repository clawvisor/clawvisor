package middleware

import (
	"context"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// AgentFromContext retrieves the authenticated agent from a request context.
// Delegates to the exported store.AgentFromContext so that cloud/enterprise
// packages can also read the agent without importing internal packages.
func AgentFromContext(ctx context.Context) *store.Agent {
	return store.AgentFromContext(ctx)
}

// RequireAgent validates an agent bearer token and injects the agent into the
// request context. Returns 401 if the token is missing or invalid.
func RequireAgent(st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				http.Error(w, `{"error":"missing authorization header","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			hash := auth.HashToken(token)
			agent, err := st.GetAgentByToken(r.Context(), hash)
			if err != nil {
				http.Error(w, `{"error":"invalid agent token","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			ctx := store.WithAgent(r.Context(), agent)
			AddLogField(ctx, "agent_id", agent.ID)
			AddLogField(ctx, "user_id", agent.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

