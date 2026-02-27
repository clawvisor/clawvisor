package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/store"
)

const (
	// AgentContextKey is the context key for the authenticated agent.
	AgentContextKey contextKey = "agent"
	// AgentRawTokenKey is the context key for the raw agent bearer token.
	AgentRawTokenKey contextKey = "agent_raw_token"
)

// AgentFromContext retrieves the authenticated agent from a request context.
func AgentFromContext(ctx context.Context) *store.Agent {
	a, _ := ctx.Value(AgentContextKey).(*store.Agent)
	return a
}

// AgentRawToken retrieves the raw bearer token from a request context.
func AgentRawToken(ctx context.Context) string {
	s, _ := ctx.Value(AgentRawTokenKey).(string)
	return s
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

			hash := agentTokenHash(token)
			agent, err := st.GetAgentByToken(r.Context(), hash)
			if err != nil {
				http.Error(w, `{"error":"invalid agent token","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), AgentContextKey, agent)
			ctx = context.WithValue(ctx, AgentRawTokenKey, token)
			AddLogField(ctx, "agent_id", agent.ID)
			AddLogField(ctx, "user_id", agent.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// agentTokenHash returns the SHA-256 hex digest of an agent token.
func agentTokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
