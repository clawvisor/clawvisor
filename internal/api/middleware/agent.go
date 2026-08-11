package middleware

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// AgentFromContext retrieves the authenticated agent from a request context.
// Delegates to the exported store.AgentFromContext so that cloud/enterprise
// packages can also read the agent without importing internal packages.
func AgentFromContext(ctx context.Context) *store.Agent {
	return store.AgentFromContext(ctx)
}

// RequireAgent validates an agent token and injects the agent into the request
// context. Returns 401 if the token is missing or invalid.
func RequireAgent(st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := agentToken(r)
			if token == "" {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing authorization header")
				return
			}

			hash := auth.HashToken(token)
			agent, err := st.GetAgentByToken(r.Context(), hash)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid agent token")
				} else {
					// Transient database error (e.g. SQLite busy) — return 503
					// so agents know to retry instead of assuming their token
					// was revoked.
					writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "temporary service error, please retry")
				}
				return
			}
			// Bound the lifetime of leaked tokens. Agents created via
			// POST /api/agents have nil TokenExpiresAt (no expiry — the
			// user owns those tokens end-to-end). MCP OAuth and relay-
			// pairing flows set a finite expiry so a leaked token has
			// finite blast radius. Treat expired same as not-found.
			if agent.TokenExpiresAt != nil && time.Now().After(*agent.TokenExpiresAt) {
				writeAuthError(w, http.StatusUnauthorized, "TOKEN_EXPIRED", "agent token has expired")
				return
			}

			ctx := store.WithAgent(r.Context(), agent)
			AddLogField(ctx, "agent_id", agent.ID)
			AddLogField(ctx, "user_id", agent.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func agentToken(r *http.Request) string {
	if token := clawvisorAgentTokenHeader(r); token != "" {
		return token
	}
	return bearerToken(r)
}
