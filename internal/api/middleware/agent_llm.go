package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// RequireAgentLLM authenticates the lite-proxy LLM endpoint. It accepts the
// agent's existing `cvis_…` token via:
//
//   - `Authorization: Bearer <token>` — OpenAI SDK convention.
//   - `x-api-key: <token>` — Anthropic SDK convention.
//   - `X-Clawvisor-Agent-Token: <token>` — Claude Code passthrough auth,
//     where Authorization must remain the user's Claude subscription OAuth token.
//
// Suitable for the LLM endpoint where the agent token rides on the SDK's
// natural auth header. For the resolver path, use RequireAgentLLMNonce
// instead — the resolver expects `Authorization` / `x-api-key` to carry
// the placeholder being swapped, and caller-auth (now a short-lived
// nonce, not the agent token) in `X-Clawvisor-Caller`.
//
// Auth bridges to the same agent-token store as RequireAgent; we don't
// mint a separate token type. The "shadow" property is automatic —
// `cvis_…` doesn't authenticate against api.anthropic.com or
// api.openai.com; it only works against this proxy.
//
// On success, attaches the resolved agent to the request context. Header
// candidates are tried in priority order — a client sending Authorization,
// x-api-key, and/or X-Clawvisor-Agent-Token with different values still
// authenticates when any one is a valid agent token. This matters for mixed-
// header clients that keep upstream OAuth in Authorization while the actual
// agent token rides in a Clawvisor-only header.
func RequireAgentLLM(st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			candidates := agentLLMTokenCandidates(r)
			if len(candidates) == 0 {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing caller-auth")
				return
			}
			var (
				agent     *store.Agent
				validTok  string
				source    string
				transient bool
			)
			for _, candidate := range candidates {
				if !strings.HasPrefix(candidate.Token, "cvis_") {
					continue
				}
				hash := auth.HashToken(candidate.Token)
				a, err := st.GetAgentByToken(r.Context(), hash)
				if err == nil {
					agent = a
					validTok = candidate.Token
					source = candidate.Source
					break
				}
				if !errors.Is(err, store.ErrNotFound) {
					transient = true
				}
			}
			if agent == nil {
				if transient {
					writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "temporary service error, please retry")
					return
				}
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid agent token")
				return
			}
			ctx := store.WithAgent(r.Context(), agent)
			ctx = withCallerToken(ctx, validTok)
			if source == agentLLMTokenSourceClawvisorHeader {
				ctx = llmproxy.WithPassthroughUpstreamAuth(ctx)
			}
			AddLogField(ctx, "agent_id", agent.ID)
			AddLogField(ctx, "user_id", agent.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAgentLLMNonce authenticates the lite-proxy resolver
// (/proxy/v1/...). The harness's resolver call carries the placeholder
// being swapped in its natural credential header (Authorization /
// x-api-key); caller-auth lives in `X-Clawvisor-Caller` and is a
// short-lived single-use nonce minted by the proxy at rewrite time.
//
// The nonce is bound to (agent_id, host, method, path). Replaying it
// against any other target fails closed. This eliminates the exposure
// of the agent's `cvis_…` token in the model's conversation context.
//
// Strict cutover: this middleware accepts only nonces (NoncePrefix).
// Raw agent tokens in X-Clawvisor-Caller no longer authenticate.
func RequireAgentLLMNonce(st store.Store, cache llmproxy.CallerNonceCache, logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			value := callerHeaderBearer(r)
			if value == "" {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing caller-auth")
				return
			}
			if !strings.HasPrefix(value, llmproxy.NoncePrefix) {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "caller-auth must be a proxy-minted nonce")
				return
			}
			if cache == nil {
				writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "caller nonce cache not configured")
				return
			}
			target := llmproxy.NonceTarget{
				Host:   strings.TrimSpace(r.Header.Get("X-Clawvisor-Target-Host")),
				Method: r.Method,
				Path:   strings.TrimPrefix(r.URL.Path, "/proxy/v1"),
			}
			agentID, err := cache.Consume(r.Context(), value, target)
			if err != nil {
				switch {
				case errors.Is(err, llmproxy.ErrNonceNotFound):
					writeAuthError(w, http.StatusUnauthorized, "NONCE_NOT_FOUND",
						"caller nonce unknown or expired")
				case errors.Is(err, llmproxy.ErrNonceTargetMismatch):
					// Misuse signal: a legitimate caller never produces
					// this. Log loudly with both target tuples so we can
					// trace the attempt.
					logger.WarnContext(r.Context(), "lite-proxy: caller nonce target mismatch",
						"actual_host", target.Host,
						"actual_method", target.Method,
						"actual_path", target.Path,
						"remote_addr", r.RemoteAddr,
					)
					writeAuthError(w, http.StatusForbidden, "NONCE_TARGET_MISMATCH",
						"caller nonce was minted for a different target")
				default:
					logger.WarnContext(r.Context(), "lite-proxy: caller nonce consume failed",
						"err", err.Error())
					writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
						"caller nonce lookup failed")
				}
				return
			}
			agent, err := st.GetAgent(r.Context(), agentID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED",
						"agent bound to nonce no longer exists")
					return
				}
				writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
					"temporary service error, please retry")
				return
			}
			ctx := store.WithAgent(r.Context(), agent)
			AddLogField(ctx, "agent_id", agent.ID)
			AddLogField(ctx, "user_id", agent.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// agentLLMTokenCandidates returns every header value that could be the
// agent token, in priority order. Callers iterate and accept the first
// one that authenticates. Returning a slice (rather than a single
// "best" value) means a client sending multiple headers with different
// values still authenticates if at least one is valid.
const (
	AgentTokenHeader                   = "X-Clawvisor-Agent-Token"
	agentLLMTokenSourceAuthorization   = "authorization"
	agentLLMTokenSourceXAPIKey         = "x-api-key"
	agentLLMTokenSourceClawvisorHeader = "x-clawvisor-agent-token"
)

type agentLLMTokenCandidate struct {
	Token  string
	Source string
}

func agentLLMTokenCandidates(r *http.Request) []agentLLMTokenCandidate {
	var out []agentLLMTokenCandidate
	if t := clawvisorAgentTokenHeader(r); t != "" {
		out = append(out, agentLLMTokenCandidate{Token: t, Source: agentLLMTokenSourceClawvisorHeader})
	}
	if t := bearerToken(r); t != "" {
		out = appendAgentLLMTokenCandidate(out, t, agentLLMTokenSourceAuthorization)
	}
	if t := strings.TrimSpace(r.Header.Get("x-api-key")); t != "" {
		out = appendAgentLLMTokenCandidate(out, t, agentLLMTokenSourceXAPIKey)
	}
	return out
}

func appendAgentLLMTokenCandidate(out []agentLLMTokenCandidate, token, source string) []agentLLMTokenCandidate {
	for _, existing := range out {
		if existing.Token == token {
			return out
		}
	}
	return append(out, agentLLMTokenCandidate{Token: token, Source: source})
}

// clawvisorAgentTokenHeader extracts out-of-band agent auth for Claude Code
// passthrough mode. Accepts either bare `cvis_...` or `Bearer cvis_...`.
func clawvisorAgentTokenHeader(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get(AgentTokenHeader))
	if v == "" {
		return ""
	}
	const prefix = "Bearer "
	if strings.HasPrefix(v, prefix) {
		return strings.TrimSpace(v[len(prefix):])
	}
	return v
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
