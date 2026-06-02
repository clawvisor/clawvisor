package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// RequireAgentLLM authenticates the lite-proxy LLM endpoint. It accepts the
// agent's existing `cvis_…` token via:
//
//   - `Authorization: Bearer <token>` — OpenAI SDK convention.
//   - `x-api-key: <token>` — Anthropic SDK convention.
//   - `X-Clawvisor-Agent-Token: <token>` — passthrough auth for clients where
//     Authorization must remain the user's upstream subscription/OAuth token.
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
			if agent.TokenExpiresAt != nil && time.Now().After(*agent.TokenExpiresAt) {
				writeAuthError(w, http.StatusUnauthorized, "TOKEN_EXPIRED", "agent token has expired")
				return
			}
			ctx := store.WithAgent(r.Context(), agent)
			ctx = withCallerToken(ctx, validTok)
			ctx = llmproxy.WithCallerAuthSource(ctx, source)
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
// (/api/proxy/...). The harness's resolver call carries the placeholder
// being swapped in its natural credential header (Authorization /
// x-api-key); caller-auth lives in `X-Clawvisor-Caller`.
//
// Two caller-auth token shapes are accepted:
//
//   - `cv-nonce-…` (CallerNonceCache): one-shot, bound to exact
//     (agent_id, host, method, path). Minted by the rewriter for each
//     model-emitted tool_use that hits an upstream API.
//   - `cv-script-…` (ScriptSessionCache): multi-use, bound to a tighter
//     capability (agent_id, placeholder, target host, allowed methods,
//     path prefixes, max uses, TTL). Minted explicitly by the agent via
//     POST /api/control/autovault/script-session for credentialed scripts.
//
// Replaying either token against any other target fails closed. Raw
// agent tokens (cvis_…) in X-Clawvisor-Caller no longer authenticate.
//
// scriptCache may be nil; the middleware then rejects script-session
// tokens with SERVICE_UNAVAILABLE rather than 401 so an operator can
// distinguish "not configured" from "bad token".
func RequireAgentLLMNonce(st store.Store, cache llmproxy.CallerNonceCache, scriptCache llmproxy.ScriptSessionCache, logger *slog.Logger) func(http.Handler) http.Handler {
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
			if strings.HasPrefix(value, llmproxy.ScriptSessionPrefix) {
				handleScriptSessionAuth(w, r, st, scriptCache, value, logger, next)
				return
			}
			if !strings.HasPrefix(value, llmproxy.NoncePrefix) {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "caller-auth must be a proxy-minted nonce or script session token")
				return
			}
			if cache == nil {
				writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "caller nonce cache not configured")
				return
			}
			// The nonce cache canonicalizes the target host (port
			// stripped) on both Mint and Consume, so passing the raw
			// header value through here is safe even when the
			// rewriter intentionally preserves :port for downstream
			// dial routing.
			target := llmproxy.NonceTarget{
				Host:   strings.TrimSpace(r.Header.Get("X-Clawvisor-Target-Host")),
				Method: r.Method,
				Path:   strings.TrimPrefix(r.URL.Path, "/api/proxy"),
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

// agentLLMTokenCandidates returns every header value that could be the
// agent token, in priority order. Callers iterate and accept the first
// one that authenticates. Returning a slice (rather than a single
// "best" value) means a client sending multiple headers with different
// values still authenticates if at least one is valid.
const (
	AgentTokenHeader                   = "X-Clawvisor-Agent-Token"
	agentLLMTokenSourceAuthorization   = llmproxy.CallerAuthSourceAuthorization
	agentLLMTokenSourceXAPIKey         = llmproxy.CallerAuthSourceXAPIKey
	agentLLMTokenSourceClawvisorHeader = llmproxy.CallerAuthSourceClawvisorHeader
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

// clawvisorAgentTokenHeader extracts out-of-band agent auth for upstream
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

// scriptSessionContextKey carries the active script-session snapshot
// forward to the resolver so the placeholder + byte caps + audit id
// are available without re-looking up the token.
type scriptSessionContextKey struct{}

type scriptSessionContext struct {
	Session llmproxy.ScriptSession
	Token   string
}

// WithScriptSession attaches a script-session snapshot to ctx. Exposed
// for tests; production code uses the middleware below.
func WithScriptSession(ctx context.Context, sess llmproxy.ScriptSession, token string) context.Context {
	return context.WithValue(ctx, scriptSessionContextKey{}, &scriptSessionContext{Session: sess, Token: token})
}

// ScriptSessionFromContext returns the active script-session
// snapshot (and its token, for RecordBytes post-response) attached by
// the middleware. (Session, token, true) when present; zero/empty/false
// when the request did not authenticate via a script-session token.
func ScriptSessionFromContext(ctx context.Context) (llmproxy.ScriptSession, string, bool) {
	v, ok := ctx.Value(scriptSessionContextKey{}).(*scriptSessionContext)
	if !ok || v == nil {
		return llmproxy.ScriptSession{}, "", false
	}
	return v.Session, v.Token, true
}

// handleScriptSessionAuth validates a `cv-script-…` caller-auth token
// against the script-session cache, attaches the agent and the session
// snapshot to the request context, and forwards. Structured 401/403/
// errors mirror the nonce path's shape and use code names from the plan
// (SCRIPT_SESSION_*).
func handleScriptSessionAuth(w http.ResponseWriter, r *http.Request, st store.Store, scriptCache llmproxy.ScriptSessionCache, token string, logger *slog.Logger, next http.Handler) {
	if scriptCache == nil {
		writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "script session cache not configured")
		return
	}
	// Script-session tokens authorize ONLY the resolver path. The
	// same middleware chain wraps several /api/control/* routes
	// (vault list, tasks, mint endpoint, etc.) — without this gate,
	// an agent could mint a script session and use the token against
	// those routes, each of which would call Authorize (reserving
	// MaxRequestBytes) but never call the post-stream RecordBytes
	// release. The reservation would leak permanently and the session
	// would exhaust in ~10 requests. Reject before Authorize so no
	// reservation is taken on the wrong route.
	if !strings.HasPrefix(r.URL.Path, "/api/proxy/") {
		writeAuthError(w, http.StatusForbidden, "SCRIPT_SESSION_WRONG_ROUTE",
			"script-session tokens authorize /api/proxy/* requests only; use a regular agent token for control-plane routes")
		return
	}
	// Discover the placeholder the script is using by inspecting the
	// caller headers. The resolver enforces ownership + bound-host
	// downstream; here we use the placeholder only to verify scope
	// against the session.
	placeholder := scriptSessionPlaceholderFromHeaders(r)
	req := llmproxy.ScriptSessionRequest{
		Host:        strings.TrimSpace(r.Header.Get("X-Clawvisor-Target-Host")),
		Method:      r.Method,
		Path:        strings.TrimPrefix(r.URL.Path, "/api/proxy"),
		Placeholder: placeholder,
	}
	sess, err := scriptCache.Authorize(r.Context(), token, req)
	if err != nil {
		// Known limitation: Authorize-time denials emit a structured
		// log line + HTTP error but NO audit_log row. The audit
		// emitter needs an authenticated agent and the
		// ScriptSessionCache's Authorize doesn't return the bound
		// session on rejection paths, so we don't have agent or
		// session details to populate a full row. Operators
		// investigating denial trails should grep the JSON log for
		// "script session" + the SCRIPT_SESSION_* code. Future work:
		// extend Authorize to return the cached session even on
		// rejection so the middleware can emit a use-row with
		// decision=deny matching the production HTTP outcome.
		switch {
		case errors.Is(err, llmproxy.ErrScriptSessionNotFound):
			writeAuthError(w, http.StatusUnauthorized, "SCRIPT_SESSION_NOT_FOUND",
				"script session unknown or revoked; mint a new one via POST /api/control/autovault/script-session")
		case errors.Is(err, llmproxy.ErrScriptSessionExpired):
			writeAuthError(w, http.StatusUnauthorized, "SCRIPT_SESSION_EXPIRED",
				"script session expired; mint a new one via POST /api/control/autovault/script-session")
		case errors.Is(err, llmproxy.ErrScriptSessionExhausted):
			writeAuthError(w, http.StatusForbidden, "SCRIPT_SESSION_EXHAUSTED",
				"script session max_uses reached; mint a new session with appropriate budget")
		case errors.Is(err, llmproxy.ErrScriptSessionBytesExceeded):
			writeAuthError(w, http.StatusForbidden, "SCRIPT_SESSION_BYTES_EXCEEDED",
				"script session response-bytes cap exhausted; mint a new session if more data is genuinely required")
		case errors.Is(err, llmproxy.ErrScriptSessionScopeMismatch):
			logger.WarnContext(r.Context(), "lite-proxy: script session scope mismatch",
				"host", req.Host, "method", req.Method, "path", req.Path,
				"placeholder", placeholder, "remote_addr", r.RemoteAddr,
			)
			writeAuthError(w, http.StatusForbidden, "SCRIPT_SESSION_SCOPE_MISMATCH",
				"request host/method/path/placeholder is outside the session's approved scope")
		default:
			logger.WarnContext(r.Context(), "lite-proxy: script session authorize failed", "err", err.Error())
			writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
				"script session lookup failed")
		}
		return
	}
	// releaseReservation runs when Authorize succeeded but we're
	// refusing to forward the request anyway (GetAgent failed,
	// agent token expired, etc.). Without it the optimistic byte
	// reservation AND the UsedCount increment Authorize took stay
	// charged against the session — and ~10 such failures
	// permanently exhaust the session's aggregate budget or burn
	// through MaxUses without a single upstream call (cubic round-3
	// P1, round-7 P2). ReleaseAuthorize undoes both. Detached
	// context so release runs even when the inbound request was
	// already cancelled.
	releaseReservation := func() {
		_ = scriptCache.ReleaseAuthorize(context.WithoutCancel(r.Context()), token)
	}
	agent, err := st.GetAgent(r.Context(), sess.AgentID)
	if err != nil {
		releaseReservation()
		if errors.Is(err, store.ErrNotFound) {
			writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED",
				"agent bound to script session no longer exists")
			return
		}
		writeAuthError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"temporary service error, please retry")
		return
	}
	if agent.TokenExpiresAt != nil && time.Now().After(*agent.TokenExpiresAt) {
		releaseReservation()
		writeAuthError(w, http.StatusUnauthorized, "TOKEN_EXPIRED", "agent token has expired")
		return
	}
	ctx := store.WithAgent(r.Context(), agent)
	ctx = WithScriptSession(ctx, sess, token)
	AddLogField(ctx, "agent_id", agent.ID)
	AddLogField(ctx, "user_id", agent.UserID)
	AddLogField(ctx, "script_session_id", sess.ID)
	next.ServeHTTP(w, r.WithContext(ctx))
}

// scriptSessionPlaceholderFromHeaders extracts the autovault_…
// placeholder from caller-credential-bearing headers. The resolver
// will re-extract and re-validate downstream; this is just for
// session-scope matching.
func scriptSessionPlaceholderFromHeaders(r *http.Request) string {
	for _, header := range []string{"Authorization", "X-Api-Key"} {
		v := strings.TrimSpace(r.Header.Get(header))
		if v == "" {
			continue
		}
		if idx := strings.Index(v, "autovault_"); idx >= 0 {
			rest := v[idx:]
			end := len(rest)
			for i, ch := range rest {
				if ch == ' ' || ch == '\t' || ch == ',' || ch == ';' || ch == '"' {
					end = i
					break
				}
			}
			return rest[:end]
		}
	}
	return ""
}
