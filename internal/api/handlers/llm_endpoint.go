package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/display"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
	"github.com/google/uuid"
)

// LLMEndpointHandler is the lite-proxy LLM termination point. It accepts
// Anthropic-/OpenAI-shaped requests authenticated by the agent's existing
// `cvis_…` token (carried in either Authorization or x-api-key), fetches
// the real upstream API key from the vault under (user_id, "anthropic" |
// "openai"), and proxies the response back. v1 is pure passthrough —
// inspector and rewriter layer in via the response-body wrap path in
// subsequent files.
type LLMEndpointHandler struct {
	Store     store.Store
	Vault     vault.Vault
	Forwarder *llmproxy.Forwarder
	Parsers   *conversation.Registry
	Logger    *slog.Logger

	// Inspector enables tool_use rewriting on the response leg. When nil,
	// the handler runs in pure passthrough mode (no inspection).
	Inspector *inspector.Inspector

	// ResolverBaseURL is the URL the rewriter redirects credentialed
	// tool_uses through (e.g. https://clawvisor.example/proxy/v1). Empty
	// disables rewriting even when Inspector is set.
	ResolverBaseURL string

	// ControlBaseURL is the daemon URL used for synthetic Clawvisor control
	// endpoint rewrites (https://clawvisor.local/control/... in tool calls).
	// Empty disables control prompt injection and control rewrites.
	ControlBaseURL string

	// AuditEmitter writes one audit_log row per /v1/* request and per
	// inspected tool_use. nil disables audit logging.
	AuditEmitter *llmproxy.AuditEmitter

	// Catalog reverse-resolves outbound (host, method, path) → (service,
	// action) for the task-scope check. Optional: when nil, task-scope
	// is not enforced for tool_use calls.
	Catalog *llmproxy.LazyServiceCatalog

	// TaskScope authorizes resolved (service, action) pairs against the
	// agent's active task scopes. Optional: when nil, task-scope is not
	// enforced.
	TaskScope llmproxy.TaskScopeChecker

	// IntentVerifier runs LLM intent verification against the matched
	// TaskAction's expected_use when the action's Verification mode
	// opts in (strict | lenient). Optional: when nil, intent verification
	// is not enforced.
	IntentVerifier llmproxy.IntentVerifier

	// PendingApprovals buffers proxy-lite tool_uses awaiting bare
	// approve/deny replies per user/agent/provider.
	PendingApprovals llmproxy.PendingApprovalCache

	// InlineTaskCreator is the handlers-side helper invoked when an
	// inline task gesture's second "approve" lands. Optional — when nil,
	// inline task approval falls back to a deny response (the model
	// can't create the task without a creator wired in). Production
	// wires this to *TasksHandler so all task validation + audit logic
	// is reused.
	InlineTaskCreator llmproxy.InlineTaskCreator

	// InlineApprovalOutcomes records the result of each inline task
	// approval so the history augmenter on later turns can re-inject
	// the correct context (success vs. failure) instead of blindly
	// claiming the task was created. Optional — when nil, the
	// augmenter skips inline-approval re-injection entirely (safer
	// than the prior unconditional "task approved" claim).
	InlineApprovalOutcomes llmproxy.InlineApprovalOutcomeStore

	// CallerNonces mints short-lived per-rewrite nonces that stand in
	// for the agent's bearer token in the rewritten tool_use. The
	// resolver-side middleware shares the same cache instance and
	// consumes one-shot on the corresponding resolver call. When nil,
	// credentialed rewrites fail closed rather than embedding the
	// agent's raw token in the model's conversation context.
	CallerNonces llmproxy.CallerNonceCache

	// TraceLogger, when non-nil, receives one JSON-line event per
	// inspector decision point for diagnostic purposes. Off by
	// default; opted in via cfg.ProxyLite.TraceLogPath.
	TraceLogger *llmproxy.TraceLogger

	// RawIOLogger, when non-nil, captures full raw HTTP bodies for
	// inbound requests, upstream responses, and the bodies returned
	// to the harness. Off by default; opted in via
	// CLAWVISOR_PROXY_LITE_RAW_LOG or cfg.ProxyLite.RawLogPath.
	// Bodies contain conversation content; the file is mode 0600 so
	// only the daemon's user can read it, but operators should still
	// avoid leaving this on outside of diagnostic sessions.
	RawIOLogger *llmproxy.RawIOLogger

	// MaxRequestBytes caps the inbound request body. Defaults to 4 MiB.
	MaxRequestBytes int64

	// MaxResponseBytes caps the upstream response body when buffering for
	// inspection. Default 32 MiB. Exceeding this returns 502
	// UPSTREAM_TOO_LARGE.
	MaxResponseBytes int64
}

// NewLLMEndpointHandler builds the handler with sensible defaults.
func NewLLMEndpointHandler(st store.Store, v vault.Vault, logger *slog.Logger) *LLMEndpointHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &LLMEndpointHandler{
		Store:                  st,
		Vault:                  v,
		Forwarder:              llmproxy.NewForwarder(v),
		Parsers:                conversation.DefaultRegistry(),
		Logger:                 logger,
		PendingApprovals:       llmproxy.NewMemoryPendingApprovalCache(10 * time.Minute),
		InlineApprovalOutcomes: llmproxy.NewMemoryInlineApprovalOutcomeStore(24 * time.Hour),
		// Default in-process nonce cache; production wires the Redis-
		// backed cache via WithCallerNonceCache. Cache instance is
		// shared with the resolver-side middleware in production.
		CallerNonces:    llmproxy.NewMemoryCallerNonceCache(5 * time.Minute),
		MaxRequestBytes: 4 << 20,
	}
}

// Messages handles `POST /v1/messages` (Anthropic) and `POST
// /v1/messages/count_tokens`. The route-selected parser dispatches to the
// Anthropic parser regardless of the inbound Host header.
func (h *LLMEndpointHandler) Messages(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r)
}

// ChatCompletions handles `POST /v1/chat/completions` (OpenAI Chat API).
func (h *LLMEndpointHandler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r)
}

// Responses handles `POST /v1/responses` (OpenAI Responses API).
func (h *LLMEndpointHandler) Responses(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r)
}

func (h *LLMEndpointHandler) serve(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := r.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = uuid.NewString()
	}

	// Per-request audit state captured at every exit path.
	var (
		auditAgent   *store.Agent
		auditAction  = "lite_proxy.unknown"
		auditStatus  int
		auditDecide  = "allow"
		auditOutcome string
		auditReason  string
		auditParams  map[string]any
	)
	defer func() {
		// One-liner summary at handler exit — visible in slog even
		// when the audit row would otherwise be lost (e.g. client
		// cancelled, store error). Pairs with the per-byte progress
		// log from ProgressReader so we can reconstruct a stalled
		// request's full timeline from logs alone.
		h.Logger.InfoContext(context.Background(), "lite-proxy request completed",
			"request_id", requestID,
			"agent_id", agentLogID(auditAgent),
			"action", auditAction,
			"http_status", auditStatus,
			"decision", auditDecide,
			"outcome", auditOutcome,
			"reason", auditReason,
			"client_cancelled", r.Context().Err() != nil,
			"total_ms", time.Since(start).Milliseconds(),
		)
		if h.AuditEmitter == nil || auditAgent == nil {
			return
		}
		provName := ""
		if p := h.Parsers.ParserForRoute(r.URL.Path); p != nil {
			provName = string(p.Name())
		}
		// Audit emission uses context.Background() rather than
		// r.Context() so a client disconnect doesn't silently drop
		// the audit row. Client cancellation IS an audit signal —
		// without this, hung/cancelled requests vanish from the
		// audit log entirely (which is what made the Openclaw
		// stalls invisible until we added the raw I/O log).
		h.AuditEmitter.LogEndpointCall(context.Background(), auditAgent, requestID, provName,
			auditAction, auditStatus, auditDecide, auditOutcome, auditReason,
			time.Since(start), auditParams)
	}()

	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		// Middleware should have rejected this; defense-in-depth.
		auditStatus = http.StatusUnauthorized
		auditDecide = "deny"
		auditOutcome = "unauthorized"
		writeJSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing agent token")
		return
	}
	auditAgent = agent

	parser := h.Parsers.ParserForRoute(r.URL.Path)
	if parser == nil {
		auditStatus = http.StatusNotFound
		auditDecide = "deny"
		auditOutcome = "not_found"
		writeJSONError(w, http.StatusNotFound, "NOT_FOUND", "unsupported route")
		return
	}
	provider := parser.Name()
	auditAction = "lite_proxy." + actionForRoute(r.URL.Path)
	auditParams = map[string]any{
		"provider": string(provider),
		"method":   r.Method,
		"path":     r.URL.Path,
		"query":    r.URL.RawQuery,
		"route":    actionForRoute(r.URL.Path),
	}

	// Read the inbound body in full. v1 doesn't stream the request side
	// (Anthropic/OpenAI don't either; bodies are bounded by tokens-of-context).
	body, err := readLimited(r.Body, h.MaxRequestBytes)
	if err != nil {
		auditStatus = http.StatusRequestEntityTooLarge
		auditDecide = "deny"
		auditOutcome = "request_too_large"
		auditReason = err.Error()
		writeJSONError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", err.Error())
		return
	}

	// Validate that the body parses for the selected provider. Surfaces
	// schema errors as a 400 before we burn an upstream call.
	if provider == conversation.ProviderAnthropic {
		sanitizedBody, sanitized, sanitizeErr := llmproxy.SanitizeAnthropicRequest(body)
		if sanitizeErr != nil {
			auditStatus = http.StatusBadRequest
			auditDecide = "deny"
			auditOutcome = "malformed_request"
			auditReason = sanitizeErr.Error()
			writeJSONError(w, http.StatusBadRequest, "MALFORMED_REQUEST", sanitizeErr.Error())
			return
		}
		if sanitized {
			body = sanitizedBody
			auditParams["anthropic_empty_text_sanitized"] = true
		}
	}
	if _, err := parser.ParseRequest(body); err != nil {
		auditStatus = http.StatusBadRequest
		auditDecide = "deny"
		auditOutcome = "malformed_request"
		auditReason = err.Error()
		writeJSONError(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
		return
	}
	// Strip the rewriter's transport details from the assistant
	// tool_use history BEFORE the model sees this request. Without
	// this, models pattern-match from their own history and start
	// emitting `cv-nonce-…` / proxy headers / rewritten URLs verbatim
	// on subsequent turns, bypassing the rewrite path entirely.
	if sanitized, sanitizeErr := llmproxy.SanitizeInboundHistory(llmproxy.SanitizeInboundRequest{
		Provider:        provider,
		Body:            body,
		ResolverBaseURL: h.ResolverBaseURL,
		ControlBaseURL:  h.ControlBaseURL,
	}); sanitizeErr != nil {
		// Sanitization is best-effort; a failure here means the
		// model sees the un-sanitized history but the request still
		// works. Log and continue.
		h.Logger.WarnContext(r.Context(), "lite-proxy inbound sanitize failed",
			"agent_id", agent.ID, "err", sanitizeErr.Error())
	} else if sanitized.Modified {
		body = sanitized.Body
		auditParams["inbound_history_sanitized"] = true
	}
	if taskRewrite, taskErr := llmproxy.RewriteTaskApprovalReply(r.Context(), llmproxy.TaskReplyRewriteRequest{
		HTTPRequest:     r,
		Provider:        provider,
		Body:            body,
		Agent:           agent,
		PendingApproval: h.PendingApprovals,
	}); taskErr != nil {
		auditStatus = http.StatusBadRequest
		auditDecide = "deny"
		auditOutcome = "malformed_request"
		auditReason = taskErr.Error()
		writeJSONError(w, http.StatusBadRequest, "MALFORMED_REQUEST", taskErr.Error())
		return
	} else if taskRewrite.Rewritten {
		body = taskRewrite.Body
		if _, err := parser.ParseRequest(body); err != nil {
			auditStatus = http.StatusBadRequest
			auditDecide = "deny"
			auditOutcome = "malformed_request"
			auditReason = err.Error()
			writeJSONError(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
			return
		}
		auditParams["approval_task_rewritten"] = true
	}

	// Inline task approval: when the user's "approve"/"deny" reply
	// resolves an awaiting_task_approval hold, create the task and
	// rewrite the user message so the LLM gets clean context (rather
	// than a synthesized cat-heredoc tool_use that confuses the model
	// into re-POSTing /control/tasks).
	inlineApprovalConsumed := false
	if inlineRewrite, inlineErr := llmproxy.RewriteInlineTaskApprovalReply(r.Context(), llmproxy.InlineApprovalRewriteRequest{
		HTTPRequest:     r,
		Provider:        provider,
		Body:            body,
		Agent:           agent,
		PendingApproval: h.PendingApprovals,
		Creator:         h.InlineTaskCreator,
		Audit:           h.AuditEmitter,
		RequestID:       requestID,
		Outcomes:        h.InlineApprovalOutcomes,
	}); inlineErr != nil {
		auditStatus = http.StatusBadRequest
		auditDecide = "deny"
		auditOutcome = "malformed_request"
		auditReason = inlineErr.Error()
		writeJSONError(w, http.StatusBadRequest, "MALFORMED_REQUEST", inlineErr.Error())
		return
	} else if inlineRewrite.Rewritten {
		body = inlineRewrite.Body
		if _, err := parser.ParseRequest(body); err != nil {
			auditStatus = http.StatusBadRequest
			auditDecide = "deny"
			auditOutcome = "malformed_request"
			auditReason = err.Error()
			writeJSONError(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
			return
		}
		inlineApprovalConsumed = true
		auditParams["inline_task_approval_rewritten"] = true
		auditParams["inline_task_outcome"] = inlineRewrite.Outcome
		if inlineRewrite.TaskID != "" {
			auditParams["inline_task_id"] = inlineRewrite.TaskID
		}
		if inlineRewrite.Reason != "" {
			auditParams["inline_task_reason"] = inlineRewrite.Reason
		}
	}

	// Persistent inline-approval context augmentation. The harness
	// records what the user typed ("approve") not our one-shot
	// rewrite ("approve [Clawvisor: ...]"), so on subsequent turns
	// the context is lost and the model duplicates work
	// (re-POSTs /control/tasks, re-emits tool_use). Walk conversation
	// history and re-inject the persistent context on every request.
	if augBody, augmented, augErr := llmproxy.AugmentApprovedInlineTasksInHistory(body, provider, h.InlineApprovalOutcomes, agent.UserID, agent.ID); augErr != nil {
		h.Logger.WarnContext(r.Context(), "lite-proxy inline task augmentation failed",
			"request_id", requestID, "agent_id", agent.ID, "err", augErr.Error())
	} else if augmented {
		body = augBody
		auditParams["inline_task_history_augmented"] = true
	}
	reqSummary := liteProxyRequestDebugSummary(provider, body)
	if h.ControlBaseURL != "" && shouldInjectLiteControlNotice(r.URL.Path, reqSummary) {
		credentialHints := h.credentialHintsForControlNotice(r.Context(), agent.UserID)
		injectedBody, injected, injectErr := llmproxy.InjectControlNoticeWithCredentialHints(provider, body, h.ControlBaseURL, reqSummary.AvailableTools, credentialHints)
		if injectErr != nil {
			auditStatus = http.StatusBadRequest
			auditDecide = "deny"
			auditOutcome = "malformed_request"
			auditReason = injectErr.Error()
			writeJSONError(w, http.StatusBadRequest, "MALFORMED_REQUEST", injectErr.Error())
			return
		}
		if injected {
			body = injectedBody
			if _, err := parser.ParseRequest(body); err != nil {
				auditStatus = http.StatusBadRequest
				auditDecide = "deny"
				auditOutcome = "malformed_request"
				auditReason = err.Error()
				writeJSONError(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
				return
			}
			reqSummary = liteProxyRequestDebugSummary(provider, body)
			auditParams["control_notice_injected"] = true
		}
	}
	auditParams["model"] = reqSummary.Model
	auditParams["stream"] = reqSummary.Stream
	auditParams["request_body_bytes"] = len(body)
	auditParams["available_tools"] = reqSummary.AvailableTools
	h.Logger.DebugContext(r.Context(), "lite-proxy request accepted",
		"request_id", requestID,
		"agent_id", agent.ID,
		"provider", string(provider),
		"method", r.Method,
		"path", r.URL.RequestURI(),
		"model", reqSummary.Model,
		"stream", reqSummary.Stream,
		"available_tools", reqSummary.AvailableTools,
		"auth_mode", liteProxyAuthMode(r),
		"body_bytes", len(body),
		"inspector_enabled", h.Inspector != nil,
		"resolver_base_url_set", h.ResolverBaseURL != "",
	)

	// Skip the regular release path when the inline rewrite already
	// consumed its hold. The rewritten user message still starts with
	// the literal "approve" verb (the rewrite preserves it so the LLM
	// can see the user's actual reply); without this guard the release
	// path would re-parse that verb, LIFO-Peek the cache, and resolve
	// some UNRELATED hold (e.g., a parallel tool-stage approval the
	// model emitted alongside the inline-task POST in the same turn).
	// A single user "approve" would then approve two distinct holds.
	if !inlineApprovalConsumed {
		if handled := h.maybeHandleLiteApprovalRelease(w, r, agent, provider, requestID, body, &auditStatus, &auditDecide, &auditOutcome, &auditReason); handled {
			return
		}
	}

	upstreamURL := ""
	if h.Forwarder != nil {
		if u, urlErr := h.Forwarder.Upstream.URL(provider, r.URL.Path); urlErr == nil {
			u.RawQuery = r.URL.RawQuery
			upstreamURL = u.String()
		} else {
			h.Logger.DebugContext(r.Context(), "lite-proxy upstream URL build failed",
				"request_id", requestID,
				"agent_id", agent.ID,
				"provider", string(provider),
				"path", r.URL.Path,
				"query", r.URL.RawQuery,
				"err", urlErr.Error(),
			)
		}
	}
	h.Logger.DebugContext(r.Context(), "lite-proxy forwarding upstream",
		"request_id", requestID,
		"agent_id", agent.ID,
		"provider", string(provider),
		"upstream_url", upstreamURL,
		"model", reqSummary.Model,
	)
	if h.RawIOLogger != nil {
		bodyStr, bodyEnc := llmproxy.EncodeBody(body)
		h.RawIOLogger.Emit(llmproxy.RawIOEvent{
			Phase:        "inbound_request",
			RequestID:    requestID,
			UserID:       agent.UserID,
			AgentID:      agent.ID,
			Provider:     string(provider),
			Method:       r.Method,
			Path:         r.URL.RequestURI(),
			Headers:      llmproxy.SafeHeaderSnapshot(r.Header),
			Body:         bodyStr,
			BodyEncoding: bodyEnc,
			BodyBytes:    len(body),
		})
	}
	forwardStart := time.Now()
	resp, err := h.Forwarder.Forward(r.Context(), agent.UserID, agent.ID, provider, r, body)
	if err != nil {
		// Distinguish client-cancelled from genuine upstream failures
		// so the audit / log signal is unambiguous when chasing
		// stalls. r.Context().Err() != nil means the inbound HTTP
		// request was closed by the client mid-flight.
		clientCancelled := r.Context().Err() != nil
		if isVaultMiss(err) {
			auditStatus = http.StatusUnauthorized
			auditDecide = "deny"
			auditOutcome = "upstream_key_missing"
			writeJSONError(w, http.StatusUnauthorized, "UPSTREAM_KEY_MISSING",
				"no upstream API key configured in vault for this provider")
			return
		}
		h.Logger.WarnContext(context.Background(), "lite-proxy forward failed",
			"request_id", requestID,
			"agent_id", agent.ID,
			"provider", string(provider),
			"err", err.Error(),
			"client_cancelled", clientCancelled,
			"forward_elapsed_ms", time.Since(forwardStart).Milliseconds(),
		)
		auditStatus = http.StatusBadGateway
		auditDecide = "deny"
		if clientCancelled {
			auditOutcome = "client_cancelled_pre_response"
		} else {
			auditOutcome = "upstream_error"
		}
		auditReason = err.Error()
		writeJSONError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed")
		return
	}
	defer resp.Body.Close()
	upstreamHeadersMs := time.Since(forwardStart).Milliseconds()
	auditStatus = resp.StatusCode
	auditOutcome = outcomeFromStatus(resp.StatusCode)
	h.Logger.InfoContext(context.Background(), "lite-proxy upstream headers received",
		"request_id", requestID,
		"agent_id", agent.ID,
		"provider", string(provider),
		"upstream_url", upstreamURL,
		"status", resp.StatusCode,
		"content_type", resp.Header.Get("Content-Type"),
		"anthropic_request_id", firstNonEmptyLog(resp.Header.Get("request-id"), resp.Header.Get("anthropic-request-id")),
		"openai_request_id", resp.Header.Get("x-request-id"),
		"ttfb_headers_ms", upstreamHeadersMs,
	)
	if auditParams == nil {
		auditParams = map[string]any{}
	}
	auditParams["ttfb_headers_ms"] = upstreamHeadersMs

	// Mirror upstream status + headers. Strip hop-by-hop. We rewrite
	// Content-Length below if postprocess mutates the body.
	for name, values := range resp.Header {
		switch http.CanonicalHeaderKey(name) {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"Te", "Trailer", "Transfer-Encoding", "Upgrade":
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}

	upstreamCT := resp.Header.Get("Content-Type")

	// Postprocess runs when we have an inspector. The resolver URL is only
	// required for credential rewrites; ordinary tool-use audit and policy
	// decisions must still run on local proxy-lite installs that do not set
	// server.public_url.
	if h.Inspector != nil {
		// Wrap the upstream body so we get TTFB / progress / final
		// stats in slog and the raw log. Reads pass through unchanged;
		// it's purely observational. Stalls in this read are the
		// most likely root cause of phantom hung requests (Anthropic
		// streams slowly + Openclaw client times out → we never log
		// upstream_response and the audit row vanishes when the
		// cancelled context falls into the deferred LogAudit call).
		progress := llmproxy.NewProgressReader(resp.Body, h.Logger, h.RawIOLogger, requestID)
		full, readErr := readResponseLimited(progress, h.MaxResponseBytes)
		bytesRead, readElapsed, readTTFB := progress.Stats()
		auditParams["upstream_body_bytes"] = bytesRead
		auditParams["upstream_read_ms"] = readElapsed.Milliseconds()
		auditParams["upstream_ttfb_body_ms"] = readTTFB.Milliseconds()
		if readErr != nil {
			clientCancelled := r.Context().Err() != nil
			h.Logger.WarnContext(context.Background(), "lite-proxy upstream read error",
				"request_id", requestID,
				"agent_id", agent.ID,
				"err", readErr.Error(),
				"bytes_read", bytesRead,
				"read_ms", readElapsed.Milliseconds(),
				"ttfb_body_ms", readTTFB.Milliseconds(),
				"client_cancelled", clientCancelled,
			)
			// Update audit fields BEFORE the JSON write — the deferred
			// audit emit at the top of serve() reads these, so without
			// the override the row would claim auditStatus=resp.StatusCode
			// (the upstream success) and auditOutcome=success from earlier.
			auditStatus = http.StatusBadGateway
			auditDecide = "deny"
			switch {
			case clientCancelled:
				auditOutcome = "client_cancelled_mid_read"
			case strings.Contains(readErr.Error(), "too large"):
				auditOutcome = "upstream_too_large"
			default:
				auditOutcome = "upstream_read_error"
			}
			auditReason = readErr.Error()
			// Clear the upstream-mirrored headers (Content-Length now
			// lies about our JSON error body, vendor request-id leaks)
			// before writing the JSON error.
			clearMirroredUpstreamHeaders(w.Header())
			writeJSONError(w, http.StatusBadGateway, "UPSTREAM_READ_ERROR", "upstream read failed")
			return
		}
		if resp.StatusCode >= 400 {
			h.Logger.DebugContext(r.Context(), "lite-proxy upstream error body",
				"request_id", requestID,
				"agent_id", agent.ID,
				"provider", string(provider),
				"status", resp.StatusCode,
				"body_preview", truncateForLog(string(full), 2048),
			)
		}
		if h.RawIOLogger != nil {
			bodyStr, bodyEnc := llmproxy.EncodeBody(full)
			h.RawIOLogger.Emit(llmproxy.RawIOEvent{
				Phase:        "upstream_response",
				RequestID:    requestID,
				UserID:       agent.UserID,
				AgentID:      agent.ID,
				Provider:     string(provider),
				Method:       r.Method,
				Path:         r.URL.RequestURI(),
				Status:       resp.StatusCode,
				ContentType:  upstreamCT,
				Headers:      llmproxy.SafeHeaderSnapshot(resp.Header),
				Body:         bodyStr,
				BodyEncoding: bodyEnc,
				BodyBytes:    len(full),
			})
		}
		callerToken := middleware.CallerTokenFromContext(r.Context())
		if callerToken == "" {
			// Fallback: extract from inbound headers — the LLM endpoint
			// uses Authorization / x-api-key for the agent's own token,
			// which is exactly the caller-auth the rewriter needs to
			// inject so the harness's outbound resolver call works.
			callerToken = inboundAgentToken(r)
		}
		opts := inspector.DefaultRewriteOpts(h.ResolverBaseURL)
		opts.CallerToken = callerToken

		var catalogIface interface {
			Resolve(host, method, path string) (llmproxy.ResolvedAction, bool)
		}
		if h.Catalog != nil {
			catalogIface = h.Catalog
		}
		candidateTasks, toolRules, egressRules := h.loadLiteProxyDecisionInputs(r.Context(), agent)
		h.Logger.DebugContext(r.Context(), "lite-proxy decision inputs loaded",
			"request_id", requestID,
			"agent_id", agent.ID,
			"provider", string(provider),
			"posture", string(liteProxyDecisionPosture(agent)),
			"candidate_tasks", len(candidateTasks),
			"tool_rules", len(toolRules),
			"egress_rules", len(egressRules),
		)
		processed := llmproxy.Postprocess(r, full, upstreamCT, llmproxy.PostprocessConfig{
			Inspector:        h.Inspector,
			RewriteOpts:      opts,
			Store:            h.Store,
			AgentUserID:      agent.UserID,
			AgentID:          agent.ID,
			Audit:            h.AuditEmitter,
			RequestID:        requestID,
			Catalog:          catalogIface,
			TaskScope:        h.TaskScope,
			IntentVerifier:   h.IntentVerifier,
			Posture:          liteProxyDecisionPosture(agent),
			CandidateTasks:   candidateTasks,
			ToolRules:        toolRules,
			EgressRules:      egressRules,
			PendingApprovals: h.PendingApprovals,
			ControlBaseURL:   h.ControlBaseURL,
			// Per-tool-use nonce minting overrides RewriteOpts.CallerToken
			// inside the credentialed rewrite path so the agent's raw
			// bearer token never enters the model's conversation context.
			CallerNonces: h.CallerNonces,
			Trace:        h.TraceLogger,
		})
		h.Logger.DebugContext(r.Context(), "lite-proxy postprocess complete",
			"request_id", requestID,
			"agent_id", agent.ID,
			"provider", string(provider),
			"status", resp.StatusCode,
			"rewritten", processed.Rewritten,
			"decisions", len(processed.Decisions),
			"skipped_reason", processed.SkippedReason,
		)
		// Fail closed when postprocess could not finish its rewrite pass.
		// A rewriter mid-body error leaves Body=nil with a non-empty
		// SkippedReason; passing the upstream body through unchanged
		// would risk leaking a literal autovault_… placeholder to the
		// model. Emit a 502 instead.
		if processed.SkippedReason != "" && len(processed.Body) == 0 {
			h.Logger.WarnContext(r.Context(), "lite-proxy postprocess failed closed",
				"agent_id", agent.ID, "reason", processed.SkippedReason)
			auditStatus = http.StatusBadGateway
			auditDecide = "deny"
			auditOutcome = "postprocess_error"
			auditReason = processed.SkippedReason
			clearMirroredUpstreamHeaders(w.Header())
			writeJSONError(w, http.StatusBadGateway, "POSTPROCESS_ERROR",
				"response postprocess failed; see clawvisor audit log")
			return
		}
		if processed.Rewritten {
			// Drop Content-Length entirely — the rewritten body's length
			// differs from upstream's. Setting it to "" leaves an empty
			// header which is malformed; Del removes it so Go writes the
			// correct length (or transfers chunked).
			w.Header().Del("Content-Length")
			// Stripping Content-Encoding because we mutated the body
			// after upstream may have compressed it; the harness should
			// not try to gunzip our plaintext.
			w.Header().Del("Content-Encoding")
		}
		if h.RawIOLogger != nil {
			bodyStr, bodyEnc := llmproxy.EncodeBody(processed.Body)
			marker := "passthrough"
			if processed.Rewritten {
				marker = "rewritten"
			}
			h.RawIOLogger.Emit(llmproxy.RawIOEvent{
				Phase:        "harness_response",
				RequestID:    requestID,
				UserID:       agent.UserID,
				AgentID:      agent.ID,
				Provider:     string(provider),
				Method:       r.Method,
				Path:         r.URL.RequestURI(),
				Status:       resp.StatusCode,
				ContentType:  processed.ContentType,
				Headers:      llmproxy.SafeHeaderSnapshot(w.Header()),
				Body:         bodyStr,
				BodyEncoding: bodyEnc,
				BodyBytes:    len(processed.Body),
				Marker:       marker,
			})
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(processed.Body)
		return
	}

	w.WriteHeader(resp.StatusCode)

	// Stream the upstream body back unchanged.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return
		}
		if readErr != nil {
			h.Logger.WarnContext(r.Context(), "lite-proxy upstream stream error",
				"agent_id", agent.ID, "err", readErr.Error())
			return
		}
	}
}

func isVaultMiss(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, vault.ErrNotFound) {
		return true
	}
	// Forwarder wraps the not-found case in its own error string for user
	// clarity; match on substring as a last resort.
	return false
}

func (h *LLMEndpointHandler) credentialHintsForControlNotice(ctx context.Context, userID string) []llmproxy.CredentialHint {
	if h.Vault == nil || userID == "" {
		return nil
	}
	keys, err := h.Vault.List(ctx, userID)
	if err != nil {
		if h.Logger != nil {
			h.Logger.WarnContext(ctx, "lite-proxy vault hint listing failed", "err", err.Error())
		}
		return nil
	}
	hints := make([]llmproxy.CredentialHint, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		hints = append(hints, llmproxy.CredentialHint{
			ID:    key,
			Label: display.ServiceName(key),
		})
	}
	return hints
}

// writeJSONError produces a uniform JSON error response.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
		"code":  code,
	})
}

// readLimited reads at most max bytes from r. Returns an error if the body
// exceeds max.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	limited := io.LimitReader(r, max+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > max {
		return nil, errors.New("request body too large")
	}
	return buf, nil
}

// readResponseLimited mirrors readLimited for upstream responses. Default
// max applies when 0 is passed (32 MiB).
func readResponseLimited(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = 32 << 20
	}
	return readLimited(r, max)
}

// actionForRoute maps a request path to an audit-log action label.
func actionForRoute(path string) string {
	switch path {
	case "/v1/messages":
		return "messages.create"
	case "/v1/messages/count_tokens":
		return "messages.count_tokens"
	case "/v1/chat/completions":
		return "chat.completions.create"
	case "/v1/responses":
		return "responses.create"
	}
	return "unknown"
}

// outcomeFromStatus turns an HTTP status code into a coarse outcome label
// for the audit row. 2xx → success, 4xx → client_error, 5xx → upstream_error.
func outcomeFromStatus(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "success"
	case status >= 400 && status < 500:
		return "client_error"
	case status >= 500:
		return "upstream_error"
	}
	return "unknown"
}

func (h *LLMEndpointHandler) loadLiteProxyDecisionInputs(ctx context.Context, agent *store.Agent) ([]*store.Task, []*store.RuntimePolicyRule, []*store.RuntimePolicyRule) {
	if h == nil || h.Store == nil || agent == nil {
		return nil, nil, nil
	}
	tasks, _, err := h.Store.ListTasks(ctx, agent.UserID, store.TaskFilter{ActiveOnly: true})
	if err != nil {
		h.Logger.WarnContext(ctx, "lite-proxy task load failed",
			"agent_id", agent.ID, "err", err.Error())
		return nil, nil, nil
	}
	candidateTasks := make([]*store.Task, 0, len(tasks))
	for _, task := range tasks {
		if task != nil && task.Status == "active" && task.AgentID == agent.ID {
			candidateTasks = append(candidateTasks, task)
		}
	}

	enabled := true
	toolRules, err := h.Store.ListRuntimePolicyRules(ctx, agent.UserID, store.RuntimePolicyRuleFilter{
		AgentID: agent.ID,
		Kind:    "tool",
		Enabled: &enabled,
	})
	if err != nil {
		h.Logger.WarnContext(ctx, "lite-proxy tool rule load failed",
			"agent_id", agent.ID, "err", err.Error())
		toolRules = nil
	}
	egressRules, err := h.Store.ListRuntimePolicyRules(ctx, agent.UserID, store.RuntimePolicyRuleFilter{
		AgentID: agent.ID,
		Kind:    "egress",
		Enabled: &enabled,
	})
	if err != nil {
		h.Logger.WarnContext(ctx, "lite-proxy egress rule load failed",
			"agent_id", agent.ID, "err", err.Error())
		egressRules = nil
	}
	return candidateTasks, toolRules, egressRules
}

func (h *LLMEndpointHandler) maybeHandleLiteApprovalRelease(w http.ResponseWriter, r *http.Request, agent *store.Agent, provider conversation.Provider, requestID string, body []byte, auditStatus *int, auditDecide, auditOutcome, auditReason *string) bool {
	candidateTasks, toolRules, egressRules := h.loadLiteProxyDecisionInputs(r.Context(), agent)
	var catalogIface interface {
		Resolve(host, method, path string) (llmproxy.ResolvedAction, bool)
	}
	if h.Catalog != nil {
		catalogIface = h.Catalog
	}
	opts := inspector.DefaultRewriteOpts(h.ResolverBaseURL)
	opts.CallerToken = inboundAgentToken(r)
	result := llmproxy.TryReleasePendingApproval(r.Context(), llmproxy.ReleaseRequest{
		HTTPRequest:     r,
		RequestID:       requestID,
		Provider:        provider,
		Body:            body,
		Agent:           agent,
		Inspector:       h.Inspector,
		RewriteOpts:     opts,
		Store:           h.Store,
		Catalog:         catalogIface,
		CandidateTasks:  candidateTasks,
		ToolRules:       toolRules,
		EgressRules:     egressRules,
		Posture:         liteProxyDecisionPosture(agent),
		IntentVerifier:  h.IntentVerifier,
		PendingApproval: h.PendingApprovals,
		Audit:           h.AuditEmitter,
		// Mint a fresh nonce at release time; the original hold predates
		// this release by an arbitrary amount, so any old nonce is gone.
		CallerNonces: h.CallerNonces,
	})
	if result.Handled {
		h.Logger.DebugContext(r.Context(), "lite-proxy approval release handled",
			"request_id", requestID,
			"agent_id", agent.ID,
			"provider", string(provider),
			"http_status", result.HTTPStatus,
			"decision", result.Decision,
			"outcome", result.Outcome,
			"reason", result.Reason,
		)
	}
	if !result.Handled {
		return false
	}
	*auditStatus = result.HTTPStatus
	*auditDecide = result.Decision
	*auditOutcome = result.Outcome
	*auditReason = result.Reason
	if len(result.Body) == 0 {
		writeJSONError(w, result.HTTPStatus, "APPROVAL_RELEASE_ERROR", result.Reason)
		return true
	}
	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("Cache-Control", "no-cache")
	if h.RawIOLogger != nil {
		bodyStr, bodyEnc := llmproxy.EncodeBody(result.Body)
		h.RawIOLogger.Emit(llmproxy.RawIOEvent{
			Phase:        "harness_response",
			RequestID:    requestID,
			UserID:       agent.UserID,
			AgentID:      agent.ID,
			Provider:     string(provider),
			Method:       r.Method,
			Path:         r.URL.RequestURI(),
			Status:       result.HTTPStatus,
			ContentType:  result.ContentType,
			Headers:      llmproxy.SafeHeaderSnapshot(w.Header()),
			Body:         bodyStr,
			BodyEncoding: bodyEnc,
			BodyBytes:    len(result.Body),
			Marker:       "synth_release_" + result.Outcome,
		})
	}
	w.WriteHeader(result.HTTPStatus)
	_, _ = io.Copy(w, bytes.NewReader(result.Body))
	return true
}

func liteProxyDecisionPosture(agent *store.Agent) runtimedecision.EvaluationPosture {
	return runtimedecision.PostureEnforce
}

type liteProxyRequestSummary struct {
	Model          string
	Stream         bool
	AvailableTools []string
}

func liteProxyRequestDebugSummary(provider conversation.Provider, body []byte) liteProxyRequestSummary {
	var summary liteProxyRequestSummary
	switch provider {
	case conversation.ProviderAnthropic:
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
			Tools  []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &req); err == nil {
			summary.Model = req.Model
			summary.Stream = req.Stream
			for _, tool := range req.Tools {
				summary.AvailableTools = appendToolName(summary.AvailableTools, tool.Name)
			}
		}
	case conversation.ProviderOpenAI:
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
			Tools  []struct {
				Type     string `json:"type"`
				Name     string `json:"name"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &req); err == nil {
			summary.Model = req.Model
			summary.Stream = req.Stream
			for _, tool := range req.Tools {
				summary.AvailableTools = appendToolName(summary.AvailableTools, firstNonEmptyLog(tool.Name, tool.Function.Name))
			}
		}
	}
	return summary
}

func shouldInjectLiteControlNotice(path string, summary liteProxyRequestSummary) bool {
	if strings.HasSuffix(path, "/count_tokens") {
		return false
	}
	return len(summary.AvailableTools) > 0
}

func appendToolName(tools []string, name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return tools
	}
	for _, existing := range tools {
		if existing == name {
			return tools
		}
	}
	return append(tools, name)
}

// agentLogID returns the agent id or "-" when no agent has been
// associated yet. Used in summary log lines to avoid printing a
// confusing empty-string field for rejected-pre-auth requests.
func agentLogID(a *store.Agent) string {
	if a == nil {
		return "-"
	}
	return a.ID
}

func liteProxyAuthMode(r *http.Request) string {
	hasBearer := strings.TrimSpace(r.Header.Get("Authorization")) != ""
	hasAPIKey := strings.TrimSpace(r.Header.Get("x-api-key")) != ""
	switch {
	case hasBearer && hasAPIKey:
		return "authorization+x-api-key"
	case hasBearer:
		return "authorization"
	case hasAPIKey:
		return "x-api-key"
	default:
		return "none"
	}
}

func truncateForLog(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...<truncated>"
}

func firstNonEmptyLog(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// clearMirroredUpstreamHeaders removes the headers we copied from the
// upstream response when an error path now wants to write a fresh JSON
// error body. Without this, Content-Length advertises the upstream's
// body size (mismatching our JSON), Content-Encoding tells the client
// to gunzip our plaintext, and vendor request-ids leak.
func clearMirroredUpstreamHeaders(h http.Header) {
	for _, name := range []string{
		"Content-Length",
		"Content-Encoding",
		"Content-Type",
		"Etag",
		"Last-Modified",
		"Cache-Control",
		"Vary",
		"Anthropic-Request-Id",
		"Request-Id",
		"X-Request-Id",
	} {
		h.Del(name)
	}
}

// inboundAgentToken extracts the cvis_… token from the inbound request's
// Authorization or x-api-key header (the SDK's natural caller-auth slot
// at the LLM endpoint). Used as a fallback to source the caller token
// for the rewriter when no dedicated middleware ran.
func inboundAgentToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token := strings.TrimSpace(h[len("Bearer "):])
		if strings.HasPrefix(token, "cvis_") {
			return token
		}
	}
	if h := strings.TrimSpace(r.Header.Get("x-api-key")); strings.HasPrefix(h, "cvis_") {
		return h
	}
	return ""
}
