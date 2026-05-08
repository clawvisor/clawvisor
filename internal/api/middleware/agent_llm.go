package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// RequireAgentLLM authenticates the lite-proxy LLM endpoint. It accepts the
// agent's existing `cvis_…` token via:
//
//   - `Authorization: Bearer <token>` — OpenAI SDK convention.
//   - `x-api-key: <token>` — Anthropic SDK convention.
//
// Suitable for the LLM endpoint where the agent token rides on the SDK's
// natural auth header. For the resolver path, use RequireAgentLLMCaller
// instead — the resolver expects `Authorization` / `x-api-key` to carry
// the placeholder being swapped, and caller-auth in `X-Clawvisor-Caller`.
//
// Auth bridges to the same agent-token store as RequireAgent; we don't
// mint a separate token type. The "shadow" property is automatic —
// `cvis_…` doesn't authenticate against api.anthropic.com or
// api.openai.com; it only works against this proxy.
//
// On success, attaches the resolved agent to the request context.
func RequireAgentLLM(st store.Store) func(http.Handler) http.Handler {
	return requireAgentLLMWithExtractor(st, agentLLMBearer)
}

// RequireAgentLLMCaller authenticates the lite-proxy resolver
// (/proxy/v1/...). The harness's resolver call carries the placeholder
// being swapped in its natural credential header (Authorization /
// x-api-key); caller-auth lives in `X-Clawvisor-Caller`. The middleware
// reads only the dedicated header to avoid the auth/payload header
// collision.
//
// The resolver-side rewriter must inject `X-Clawvisor-Caller: Bearer
// <agent-token>` into rewritten tool_use headers so the harness sends it.
func RequireAgentLLMCaller(st store.Store) func(http.Handler) http.Handler {
	return requireAgentLLMWithExtractor(st, callerHeaderBearer)
}

func requireAgentLLMWithExtractor(st store.Store, extract func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extract(r)
			if token == "" {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing caller-auth")
				return
			}
			if !strings.HasPrefix(token, "cvis_") {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "expected agent token (cvis_…)")
				return
			}

			hash := auth.HashToken(token)
			agent, err := st.GetAgentByToken(r.Context(), hash)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid agent token")
				} else {
					writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "temporary service error, please retry")
				}
				return
			}

			ctx := store.WithAgent(r.Context(), agent)
			ctx = withCallerToken(ctx, token)
			AddLogField(ctx, "agent_id", agent.ID)
			AddLogField(ctx, "user_id", agent.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// agentLLMBearer extracts the agent token from the LLM endpoint's
// natural-header conventions: Authorization or x-api-key.
func agentLLMBearer(r *http.Request) string {
	if t := bearerToken(r); t != "" {
		return t
	}
	if t := strings.TrimSpace(r.Header.Get("x-api-key")); t != "" {
		return t
	}
	return ""
}

// callerHeaderBearer extracts the agent token from `X-Clawvisor-Caller`.
// Accepts either bare `cvis_…` or `Bearer cvis_…` for ergonomics.
func callerHeaderBearer(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("X-Clawvisor-Caller"))
	if v == "" {
		return ""
	}
	const prefix = "Bearer "
	if strings.HasPrefix(v, prefix) {
		return strings.TrimSpace(v[len(prefix):])
	}
	return v
}

// callerTokenContextKey carries the raw caller token forward so handlers
// (e.g. the LLM endpoint's rewriter) can inject it into rewritten tool_use
// headers as `X-Clawvisor-Caller`.
type callerTokenContextKey struct{}

func withCallerToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, callerTokenContextKey{}, token)
}

// CallerTokenFromContext returns the raw `cvis_…` token attached by the
// middleware, or empty string when not present.
func CallerTokenFromContext(ctx context.Context) string {
	t, _ := ctx.Value(callerTokenContextKey{}).(string)
	return t
}
