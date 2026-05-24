package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	runtimeautovault "github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
	"github.com/google/uuid"
)

// LLMEndpointHandler is the lite-proxy LLM termination point. It accepts
// Anthropic-/OpenAI-shaped requests authenticated by the agent's existing
// `cvis_…` token (carried in Authorization, x-api-key, or
// X-Clawvisor-Agent-Token for upstream-auth passthrough), fetches or preserves
// upstream auth, and proxies the response back. The provider-compatible
// /api/v1 routes are passthrough endpoints; inspector and rewriter layer in via
// the response-body wrap path in subsequent files.
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
	// tool_uses through (e.g. https://clawvisor.example/api/proxy). Empty
	// disables rewriting even when Inspector is set.
	ResolverBaseURL string

	// ControlBaseURL is the daemon URL used for synthetic Clawvisor control
	// endpoint rewrites (https://clawvisor.local/control/... in tool calls).
	// Empty disables control prompt injection and control rewrites.
	ControlBaseURL string

	// AuditEmitter writes one audit_log row per /api/v1/* request and per
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

	// PendingSecrets buffers inbound requests that appear to contain raw
	// secrets until the user decides whether to vault, discard, allow
	// once, or mark them as non-secrets.
	PendingSecrets llmproxy.PendingSecretDecisionCache

	// SecretAdjudicator classifies ambiguous inbound candidates before
	// proxy-lite interrupts the user. Deterministic findings do not need
	// adjudication.
	SecretAdjudicator runtimeautovault.SecretAdjudicator

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

	// TaskCheckouts stores the current task focus for an agent. The decision
	// layer treats this as a preference among already-valid task candidates.
	TaskCheckouts llmproxy.TaskCheckoutStore

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

	// TaskRiskAssessor evaluates the runtime envelope of an inline task
	// gesture at approval time. Optional — when nil, the inline approval
	// prompt falls back to the deterministic envelope-shape policy only.
	TaskRiskAssessor taskrisk.Assessor

	// RawIOLogger, when non-nil, captures full raw HTTP bodies for
	// inbound requests, upstream responses, and the bodies returned
	// to the harness. Off by default; opted in via
	// CLAWVISOR_PROXY_LITE_RAW_LOG or cfg.ProxyLite.RawLogPath.
	// Bodies contain conversation content; the file is mode 0600 so
	// only the daemon's user can read it, but operators should still
	// avoid leaving this on outside of diagnostic sessions.
	RawIOLogger *llmproxy.RawIOLogger

	// MaxRequestBytes caps the inbound request body. Defaults to 34 MiB —
	// 2 MiB of headroom above Anthropic's 32 MB Messages API cap (OpenAI's
	// is ~25 MB), so the proxy never rejects a request the upstream would
	// have accepted.
	MaxRequestBytes int64

	// MaxResponseBytes caps the upstream response body when buffering for
	// inspection. Default 32 MiB. Exceeding this returns 502
	// UPSTREAM_TOO_LARGE.
	MaxResponseBytes int64

	defaultToolRulesMu   sync.Mutex
	defaultToolRulesSeen map[string]map[string]struct{}
}

const defaultToolRulesSeenMaxAgents = 10000

var errSecretVaultNameConflict = errors.New("vault item already exists with a different value")

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
		PendingSecrets:         llmproxy.NewMemoryPendingSecretDecisionCache(10 * time.Minute),
		InlineApprovalOutcomes: llmproxy.NewMemoryInlineApprovalOutcomeStore(24 * time.Hour),
		TaskCheckouts:          llmproxy.NewMemoryTaskCheckoutStore(24 * time.Hour),
		// Default in-process nonce cache; production wires the Redis-
		// backed cache via WithCallerNonceCache. Cache instance is
		// shared with the resolver-side middleware in production.
		CallerNonces:         llmproxy.NewMemoryCallerNonceCache(5 * time.Minute),
		MaxRequestBytes:      34 << 20,
		defaultToolRulesSeen: map[string]map[string]struct{}{},
	}
}

// Messages handles `POST /api/v1/messages` (Anthropic) and `POST
// /api/v1/messages/count_tokens`. The route-selected parser dispatches to the
// Anthropic parser regardless of the inbound Host header.
func (h *LLMEndpointHandler) Messages(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r)
}

// ChatCompletions handles `POST /api/v1/chat/completions` (OpenAI Chat API).
func (h *LLMEndpointHandler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r)
}

// Responses handles `POST /api/v1/responses` (OpenAI Responses API).
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
	passthrough := h.activeLitePassthrough(r.Context(), agent)
	if passthrough.Enabled {
		auditParams["passthrough"] = true
		auditParams["passthrough_rule_id"] = passthrough.RuleID
		auditParams["passthrough_reason"] = passthrough.Reason
		if passthrough.ExpiresAt != nil {
			auditParams["passthrough_expires_at"] = passthrough.ExpiresAt.Format(time.RFC3339Nano)
		}
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
	if h.RawIOLogger != nil {
		bodyStr, bodyEnc := llmproxy.EncodeBody(body)
		h.RawIOLogger.Emit(llmproxy.RawIOEvent{
			Phase:        "proxy_received_request",
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
			Marker:       "before_preprocess",
		})
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
	if passthrough.Enabled {
		auditParams["request_body_bytes"] = len(body)
		h.Logger.InfoContext(r.Context(), "lite-proxy passthrough mode active",
			"request_id", requestID,
			"agent_id", agent.ID,
			"rule_id", passthrough.RuleID,
			"expires_at", passthrough.ExpiresAt,
		)
		h.forwardLitePassthrough(w, r, agent, provider, requestID, body, &auditStatus, &auditDecide, &auditOutcome, &auditReason, auditParams)
		return
	}
	decisionExtraSuppressed := map[string]struct{}{}
	liteProxySecretDetectionDisabled := agentLiteProxySecretDetectionDisabled(agent)
	if liteProxySecretDetectionDisabled {
		auditParams["lite_proxy_secret_detection_disabled"] = true
	} else {
		if decisionBody, decision, extraSuppressed, handled := h.maybeHandleLiteSecretDecision(w, r, agent, provider, requestID, body, auditParams, &auditStatus, &auditDecide, &auditOutcome, &auditReason); handled {
			if len(decisionBody) == 0 {
				return
			}
			body = decisionBody
			if _, err := parser.ParseRequest(body); err != nil {
				auditStatus = http.StatusBadRequest
				auditDecide = "deny"
				auditOutcome = "malformed_request"
				auditReason = err.Error()
				writeJSONError(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
				return
			}
			auditParams["secret_decision"] = string(decision)
			decisionExtraSuppressed = extraSuppressed
		}
	}
	if processed, held := h.preprocessLiteSecretBody(w, r, agent, provider, requestID, body, decisionExtraSuppressed, liteProxySecretDetectionDisabled, auditParams, &auditStatus, &auditDecide, &auditOutcome, &auditReason); held {
		return
	} else {
		body = processed
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
	// Extract per-conversation identifier from the inbound body once. It
	// scopes pending approvals + task checkout to a single conversation
	// when multiple sessions share a Clawvisor token (Conductor workspaces,
	// sub-agents, multiple Claude Code installs). Threaded through every
	// downstream rewrite + the release path. Empty falls back to the
	// pre-conversation-scoping behavior, so older clients that don't
	// surface a conversation ID continue working.
	conversationID := conversation.ConversationID(r, provider, body)
	if conversationID != "" {
		auditParams["conversation_id"] = conversationID
	}
	if taskRewrite, taskErr := llmproxy.RewriteTaskApprovalReply(r.Context(), llmproxy.TaskReplyRewriteRequest{
		HTTPRequest:     r,
		Provider:        provider,
		Body:            body,
		Agent:           agent,
		ConversationID:  conversationID,
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
	// into re-POSTing /api/control/tasks).
	inlineApprovalConsumed := false
	if inlineRewrite, inlineErr := llmproxy.RewriteInlineTaskApprovalReply(r.Context(), llmproxy.InlineApprovalRewriteRequest{
		HTTPRequest:     r,
		Provider:        provider,
		Body:            body,
		Agent:           agent,
		ConversationID:  conversationID,
		PendingApproval: h.PendingApprovals,
		Creator:         h.InlineTaskCreator,
		Audit:           h.AuditEmitter,
		RequestID:       requestID,
		Outcomes:        h.InlineApprovalOutcomes,
		Checkouts:       h.TaskCheckouts,
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
		if inlineRewrite.CheckedOut {
			auditParams["inline_task_checked_out"] = true
		}
		if inlineRewrite.Reason != "" {
			auditParams["inline_task_reason"] = inlineRewrite.Reason
		}
	}

	// Persistent inline-approval context augmentation. The harness
	// records what the user typed ("approve") not our one-shot
	// rewrite ("approve [Clawvisor: ...]"), so on subsequent turns
	// the context is lost and the model duplicates work
	// (re-POSTs /api/control/tasks, re-emits tool_use). Walk conversation
	// history and re-inject the persistent context on every request.
	if augBody, augmented, augErr := llmproxy.AugmentApprovedInlineTasksInHistory(body, provider, h.InlineApprovalOutcomes, agent.UserID, agent.ID); augErr != nil {
		h.Logger.WarnContext(r.Context(), "lite-proxy inline task augmentation failed",
			"request_id", requestID, "agent_id", agent.ID, "err", augErr.Error())
	} else if augmented {
		body = augBody
		auditParams["inline_task_history_augmented"] = true
	}
	reqSummary := liteProxyRequestDebugSummary(provider, body)
	h.ensureDefaultToolRules(r.Context(), agent, reqSummary.AvailableTools)
	if h.ControlBaseURL != "" && shouldInjectLiteControlNotice(r.URL.Path, reqSummary) {
		// Notice injection is best-effort UX; a store error here should
		// not fail-close the request because no authorization decision
		// is being made. Authorization-relevant call sites below check
		// the error and refuse.
		_, noticeToolRules, _, noticeLoadErr := h.loadLiteProxyDecisionInputs(r.Context(), agent)
		if noticeLoadErr != nil {
			h.Logger.WarnContext(r.Context(), "lite-proxy notice injection skipped: decision-input load failed",
				"agent_id", agent.ID, "err", noticeLoadErr.Error())
			noticeToolRules = nil
		}
		injectedBody, injected, injectErr := llmproxy.InjectControlNoticeWithPolicy(provider, body, h.ControlBaseURL, reqSummary.AvailableTools, noticeToolRules)
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
	// consumed its hold. Without this guard, a future change that
	// leaves any parseable approval text in the rewritten body could
	// let the release path resolve an unrelated hold (e.g., a parallel
	// tool-stage approval emitted alongside the inline-task POST in
	// the same turn). A single user "approve" must only resolve one
	// hold.
	if !inlineApprovalConsumed {
		if handled := h.maybeHandleLiteApprovalRelease(w, r, agent, provider, requestID, conversationID, body, &auditStatus, &auditDecide, &auditOutcome, &auditReason); handled {
			return
		}
	}
	if stripped, stripErr := llmproxy.StripSyntheticApprovalHistory(llmproxy.SyntheticApprovalHistoryStripRequest{
		Provider: provider,
		Body:     body,
	}); stripErr != nil {
		h.Logger.WarnContext(r.Context(), "lite-proxy synthetic approval history strip failed",
			"request_id", requestID, "agent_id", agent.ID, "err", stripErr.Error())
	} else if stripped.Modified {
		body = stripped.Body
		auditParams["synthetic_approval_history_stripped"] = true
		reqSummary = liteProxyRequestDebugSummary(provider, body)
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
		candidateTasks, toolRules, egressRules, decisionLoadErr := h.loadLiteProxyDecisionInputs(r.Context(), agent)
		if decisionLoadErr != nil {
			// Fail closed: postprocess gates EvaluateAuthorization on at
			// least one input being non-nil; returning nils on a
			// transient store outage would let credentialless tool_uses
			// pass through unchecked. Surface a 503 so the harness can
			// retry rather than silently weaken enforcement.
			h.Logger.WarnContext(r.Context(), "lite-proxy decision-input load failed; failing closed",
				"request_id", requestID, "agent_id", agent.ID, "err", decisionLoadErr.Error())
			auditStatus = http.StatusServiceUnavailable
			auditDecide = "deny"
			auditOutcome = "decision_input_load_failed"
			auditReason = decisionLoadErr.Error()
			clearMirroredUpstreamHeaders(w.Header())
			writeJSONError(w, http.StatusServiceUnavailable, "DECISION_INPUT_UNAVAILABLE",
				"authorization inputs unavailable; please retry")
			return
		}
		preferredTaskID, preferredTaskErr := h.checkedOutTaskID(r.Context(), agent, conversationID, candidateTasks)
		if preferredTaskErr != nil {
			h.Logger.WarnContext(r.Context(), "lite-proxy task checkout lookup failed; continuing without preferred task",
				"request_id", requestID, "agent_id", agent.ID, "err", preferredTaskErr.Error())
			auditParams["task_checkout_unavailable"] = true
			preferredTaskID = ""
		}
		h.Logger.DebugContext(r.Context(), "lite-proxy decision inputs loaded",
			"request_id", requestID,
			"agent_id", agent.ID,
			"provider", string(provider),
			"posture", string(liteProxyDecisionPosture(agent)),
			"candidate_tasks", len(candidateTasks),
			"tool_rules", len(toolRules),
			"egress_rules", len(egressRules),
			"preferred_task_id", preferredTaskID,
		)
		// Conversation-based auto-approval inputs: human turns from the
		// inbound transcript and the agent's per-runtime threshold.
		// Both are best-effort — extraction yields []string{} on a
		// malformed body, and an unset threshold collapses to "off",
		// which makes the gate refuse to fire. Either fallback
		// preserves existing behavior (human approval prompt) rather
		// than risking a spurious auto-approve.
		recentTurns := llmproxy.ExtractRecentHumanTurns(llmproxy.ExtractHumanTurnsRequest{
			Provider: provider,
			Body:     body,
		})
		autoApproveThreshold := agentConversationAutoApproveThreshold(agent)
		processed := llmproxy.Postprocess(r, full, upstreamCT, llmproxy.PostprocessConfig{
			Inspector:        h.Inspector,
			RewriteOpts:      opts,
			Store:            h.Store,
			AgentUserID:      agent.UserID,
			AgentID:          agent.ID,
			ConversationID:   conversationID,
			Audit:            h.AuditEmitter,
			RequestID:        requestID,
			Catalog:          catalogIface,
			TaskScope:        h.TaskScope,
			IntentVerifier:   h.IntentVerifier,
			Posture:          liteProxyDecisionPosture(agent),
			CandidateTasks:   candidateTasks,
			ToolRules:        toolRules,
			EgressRules:      egressRules,
			PreferredTaskID:  preferredTaskID,
			PendingApprovals: h.PendingApprovals,
			ControlBaseURL:   h.ControlBaseURL,
			// Per-tool-use nonce minting overrides RewriteOpts.CallerToken
			// inside the credentialed rewrite path so the agent's raw
			// bearer token never enters the model's conversation context.
			CallerNonces:                     h.CallerNonces,
			Trace:                            h.TraceLogger,
			TaskRiskAssessor:                 h.taskRiskBridge(),
			AgentName:                        agent.Name,
			RecentUserTurns:                  recentTurns,
			ConversationAutoApproveThreshold: autoApproveThreshold,
			InlineTaskCreator:                h.InlineTaskCreator,
			Checkouts:                        h.TaskCheckouts,
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

type limitedCaptureWriter struct {
	max int64
	buf bytes.Buffer
}

func newLimitedCaptureWriter(max int64) *limitedCaptureWriter {
	if max <= 0 {
		max = 32 << 20
	}
	return &limitedCaptureWriter{max: max}
}

func (w *limitedCaptureWriter) Write(p []byte) (int, error) {
	if w == nil {
		return len(p), nil
	}
	remaining := w.max - int64(w.buf.Len())
	if remaining > 0 {
		if int64(len(p)) < remaining {
			_, _ = w.buf.Write(p)
		} else {
			_, _ = w.buf.Write(p[:remaining])
		}
	}
	return len(p), nil
}

func (w *limitedCaptureWriter) Bytes() []byte {
	if w == nil {
		return nil
	}
	return w.buf.Bytes()
}

// actionForRoute maps a request path to an audit-log action label.
func actionForRoute(path string) string {
	path = strings.TrimPrefix(path, "/api")
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

// loadLiteProxyDecisionInputs loads the per-request authorization inputs
// (active tasks, tool rules, egress rules) for the agent's user. Returns
// a non-nil error if any of the underlying store reads fails. Callers in
// the authorization path MUST fail closed on error — postprocess gates
// EvaluateAuthorization on at least one of the three slices being
// non-nil, so silently substituting nil on error would skip enforcement
// and let credentialed-or-credentialless tool calls pass through during
// a transient store outage.
//
// On success, every returned slice is non-nil (possibly empty) so the
// downstream gate fires even for agents with zero configured rules and
// no active tasks; EvaluateAuthorization then issues a NeedsApproval
// verdict via SourceTaskScopeMissing, matching the configured posture.
func (h *LLMEndpointHandler) loadLiteProxyDecisionInputs(ctx context.Context, agent *store.Agent) ([]*store.Task, []*store.RuntimePolicyRule, []*store.RuntimePolicyRule, error) {
	if h == nil || h.Store == nil || agent == nil {
		return nil, nil, nil, nil
	}
	tasks, _, err := h.Store.ListTasks(ctx, agent.UserID, store.TaskFilter{ActiveOnly: true})
	if err != nil {
		h.Logger.WarnContext(ctx, "lite-proxy task load failed",
			"agent_id", agent.ID, "err", err.Error())
		return nil, nil, nil, fmt.Errorf("list tasks: %w", err)
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
		return nil, nil, nil, fmt.Errorf("list tool rules: %w", err)
	}
	if toolRules == nil {
		toolRules = []*store.RuntimePolicyRule{}
	}
	egressRules, err := h.Store.ListRuntimePolicyRules(ctx, agent.UserID, store.RuntimePolicyRuleFilter{
		AgentID: agent.ID,
		Kind:    "egress",
		Enabled: &enabled,
	})
	if err != nil {
		h.Logger.WarnContext(ctx, "lite-proxy egress rule load failed",
			"agent_id", agent.ID, "err", err.Error())
		return nil, nil, nil, fmt.Errorf("list egress rules: %w", err)
	}
	if egressRules == nil {
		egressRules = []*store.RuntimePolicyRule{}
	}
	return candidateTasks, toolRules, egressRules, nil
}

func (h *LLMEndpointHandler) checkedOutTaskID(ctx context.Context, agent *store.Agent, conversationID string, candidateTasks []*store.Task) (string, error) {
	if h == nil || h.TaskCheckouts == nil || agent == nil {
		return "", nil
	}
	// Scoped lookup first: a checkout written by inline-task approval
	// in this conversation should win. If the scoped bucket misses,
	// fall back to the legacy (user, agent)-only bucket — that's where
	// `POST /control/task/checkout` writes, since the control endpoint
	// has no per-turn conversation context. Without this fallback, a
	// manually-selected task would never be preferred for any
	// conversation that surfaces a non-empty ConversationID.
	scopedKey := llmproxy.TaskCheckoutKey{
		UserID:         agent.UserID,
		AgentID:        agent.ID,
		ConversationID: conversationID,
	}
	if id, err := h.resolveCheckedOutTaskID(ctx, scopedKey, agent, candidateTasks); err != nil || id != "" {
		return id, err
	}
	if conversationID == "" {
		// Already checked the legacy bucket above.
		return "", nil
	}
	legacyKey := llmproxy.TaskCheckoutKey{
		UserID:  agent.UserID,
		AgentID: agent.ID,
	}
	return h.resolveCheckedOutTaskID(ctx, legacyKey, agent, candidateTasks)
}

// resolveCheckedOutTaskID looks up a single TaskCheckoutKey, returns
// the checked-out task ID when it still matches an active candidate
// task for this agent, and clears the entry when it's stale. Returning
// ("", nil) means the bucket either had no entry or had a stale entry
// (which is now cleared) — the caller should treat both the same.
func (h *LLMEndpointHandler) resolveCheckedOutTaskID(ctx context.Context, key llmproxy.TaskCheckoutKey, agent *store.Agent, candidateTasks []*store.Task) (string, error) {
	checkout, ok, err := h.TaskCheckouts.Get(ctx, key)
	if err != nil || !ok || strings.TrimSpace(checkout.TaskID) == "" {
		return "", err
	}
	for _, task := range candidateTasks {
		if task != nil && task.ID == checkout.TaskID && task.Status == "active" && task.AgentID == agent.ID {
			return checkout.TaskID, nil
		}
	}
	if err := h.TaskCheckouts.Clear(ctx, key); err != nil {
		return "", err
	}
	return "", nil
}

func (h *LLMEndpointHandler) ensureDefaultToolRules(ctx context.Context, agent *store.Agent, availableTools []string) {
	if h == nil || h.Store == nil || agent == nil {
		return
	}
	toSync := h.unseededDefaultTools(agent.ID, availableTools)
	if len(toSync) == 0 {
		return
	}
	if err := ensureDefaultToolRules(ctx, h.Store, agent, toSync); err != nil {
		h.Logger.WarnContext(ctx, "lite-proxy default tool rule sync failed",
			"agent_id", agent.ID, "err", err.Error())
		return
	}
	h.markDefaultToolsSeeded(agent.ID, toSync)
}

func (h *LLMEndpointHandler) unseededDefaultTools(agentID string, availableTools []string) []string {
	h.defaultToolRulesMu.Lock()
	defer h.defaultToolRulesMu.Unlock()
	if h.defaultToolRulesSeen == nil {
		h.defaultToolRulesSeen = map[string]map[string]struct{}{}
	}
	if len(h.defaultToolRulesSeen) > defaultToolRulesSeenMaxAgents {
		h.defaultToolRulesSeen = map[string]map[string]struct{}{}
	}
	seen := h.defaultToolRulesSeen[agentID]
	out := make([]string, 0, len(availableTools))
	queued := map[string]struct{}{}
	for _, toolName := range availableTools {
		toolName = strings.TrimSpace(toolName)
		if toolName == "" || !inspector.IsDefaultAllowedTool(toolName) {
			continue
		}
		key := strings.ToLower(toolName)
		if _, ok := queued[key]; ok {
			continue
		}
		if seen != nil {
			if _, ok := seen[key]; ok {
				continue
			}
		}
		queued[key] = struct{}{}
		out = append(out, toolName)
	}
	return out
}

func (h *LLMEndpointHandler) markDefaultToolsSeeded(agentID string, toolNames []string) {
	h.defaultToolRulesMu.Lock()
	defer h.defaultToolRulesMu.Unlock()
	if h.defaultToolRulesSeen == nil {
		h.defaultToolRulesSeen = map[string]map[string]struct{}{}
	}
	if _, ok := h.defaultToolRulesSeen[agentID]; !ok && len(h.defaultToolRulesSeen) >= defaultToolRulesSeenMaxAgents {
		h.defaultToolRulesSeen = map[string]map[string]struct{}{}
	}
	seen := h.defaultToolRulesSeen[agentID]
	if seen == nil {
		seen = map[string]struct{}{}
		h.defaultToolRulesSeen[agentID] = seen
	}
	for _, toolName := range toolNames {
		toolName = strings.TrimSpace(toolName)
		if toolName == "" || !inspector.IsDefaultAllowedTool(toolName) {
			continue
		}
		seen[strings.ToLower(toolName)] = struct{}{}
	}
}

func ensureDefaultToolRules(ctx context.Context, st store.Store, agent *store.Agent, availableTools []string) error {
	if st == nil || agent == nil || len(availableTools) == 0 {
		return nil
	}
	existing, err := st.ListRuntimePolicyRules(ctx, agent.UserID, store.RuntimePolicyRuleFilter{
		AgentID: agent.ID,
		Kind:    "tool",
		Limit:   1000,
	})
	if err != nil {
		return err
	}
	hasSimpleRule := map[string]bool{}
	for _, rule := range existing {
		if rule == nil || strings.TrimSpace(rule.ToolName) == "" || !isSimpleToolControlRule(rule) {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(rule.ToolName))
		if rule.AgentID != nil && *rule.AgentID == agent.ID {
			hasSimpleRule[key] = true
			continue
		}
		if rule.AgentID == nil && rule.Source == "system" && rule.Action == "allow" && inspector.IsDefaultAllowedTool(rule.ToolName) {
			continue
		}
		if rule.AgentID == nil {
			hasSimpleRule[key] = true
		}
	}
	for _, toolName := range availableTools {
		toolName = strings.TrimSpace(toolName)
		if toolName == "" || !inspector.IsDefaultAllowedTool(toolName) {
			continue
		}
		key := strings.ToLower(toolName)
		if hasSimpleRule[key] {
			continue
		}
		agentID := agent.ID
		rule := &store.RuntimePolicyRule{
			ID:         uuid.NewSHA1(uuid.NameSpaceURL, []byte("lite-proxy-default-tool:"+agent.UserID+":"+agent.ID+":"+key)).String(),
			UserID:     agent.UserID,
			AgentID:    &agentID,
			Kind:       "tool",
			Action:     "allow",
			ToolName:   toolName,
			InputShape: json.RawMessage(`{}`),
			Reason:     "Default allow for tool " + toolName,
			Source:     "system",
			Enabled:    true,
		}
		if err := st.CreateRuntimePolicyRule(ctx, rule); err != nil && !errors.Is(err, store.ErrConflict) {
			return err
		}
		hasSimpleRule[key] = true
	}
	return nil
}

func (h *LLMEndpointHandler) activeLitePassthrough(ctx context.Context, agent *store.Agent) runtimePassthroughState {
	if h == nil || h.Store == nil || agent == nil {
		return runtimePassthroughState{}
	}
	enabled := true
	rules, err := h.Store.ListRuntimePolicyRules(ctx, agent.UserID, store.RuntimePolicyRuleFilter{
		AgentID: agent.ID,
		Kind:    runtimePassthroughKind,
		Enabled: &enabled,
		Limit:   100,
	})
	if err != nil {
		h.Logger.WarnContext(ctx, "lite-proxy passthrough load failed",
			"agent_id", agent.ID, "err", err.Error())
		return runtimePassthroughState{}
	}
	return activePassthroughFromRules(rules, agent.ID, time.Now().UTC())
}

func (h *LLMEndpointHandler) forwardLitePassthrough(w http.ResponseWriter, r *http.Request, agent *store.Agent, provider conversation.Provider, requestID string, body []byte, auditStatus *int, auditDecide, auditOutcome, auditReason *string, auditParams map[string]any) {
	upstreamURL := ""
	if h.Forwarder != nil {
		if u, urlErr := h.Forwarder.Upstream.URL(provider, r.URL.Path); urlErr == nil {
			u.RawQuery = r.URL.RawQuery
			upstreamURL = u.String()
		}
	}
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
			Marker:       "break_glass_passthrough",
		})
	}
	forwardStart := time.Now()
	resp, err := h.Forwarder.Forward(r.Context(), agent.UserID, agent.ID, provider, r, body)
	if err != nil {
		clientCancelled := r.Context().Err() != nil
		h.Logger.WarnContext(context.Background(), "lite-proxy passthrough forward failed",
			"request_id", requestID,
			"agent_id", agent.ID,
			"provider", string(provider),
			"err", err.Error(),
			"client_cancelled", clientCancelled,
			"forward_elapsed_ms", time.Since(forwardStart).Milliseconds(),
		)
		if isVaultMiss(err) {
			*auditStatus = http.StatusUnauthorized
			*auditDecide = "deny"
			*auditOutcome = "upstream_key_missing"
			writeJSONError(w, http.StatusUnauthorized, "UPSTREAM_KEY_MISSING", "no upstream API key configured in vault for this provider")
			return
		}
		*auditStatus = http.StatusBadGateway
		*auditDecide = "deny"
		if clientCancelled {
			*auditOutcome = "client_cancelled_pre_response"
		} else {
			*auditOutcome = "upstream_error"
		}
		*auditReason = err.Error()
		writeJSONError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed")
		return
	}
	defer resp.Body.Close()
	*auditStatus = resp.StatusCode
	*auditDecide = "allow"
	*auditOutcome = outcomeFromStatus(resp.StatusCode)
	upstreamHeadersMs := time.Since(forwardStart).Milliseconds()
	if auditParams != nil {
		auditParams["ttfb_headers_ms"] = upstreamHeadersMs
	}
	h.Logger.InfoContext(context.Background(), "lite-proxy passthrough upstream headers received",
		"request_id", requestID,
		"agent_id", agent.ID,
		"provider", string(provider),
		"upstream_url", upstreamURL,
		"status", resp.StatusCode,
		"ttfb_headers_ms", upstreamHeadersMs,
	)
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
	if h.RawIOLogger != nil {
		capture := newLimitedCaptureWriter(h.MaxResponseBytes)
		w.WriteHeader(resp.StatusCode)
		_, copyErr := io.Copy(io.MultiWriter(w, capture), resp.Body)
		if copyErr != nil {
			*auditOutcome = "upstream_read_error"
			*auditReason = copyErr.Error()
		}
		full := capture.Bytes()
		bodyStr, bodyEnc := llmproxy.EncodeBody(full)
		h.RawIOLogger.Emit(llmproxy.RawIOEvent{
			Phase:        "harness_response",
			RequestID:    requestID,
			UserID:       agent.UserID,
			AgentID:      agent.ID,
			Provider:     string(provider),
			Method:       r.Method,
			Path:         r.URL.RequestURI(),
			Status:       resp.StatusCode,
			ContentType:  resp.Header.Get("Content-Type"),
			Headers:      llmproxy.SafeHeaderSnapshot(w.Header()),
			Body:         bodyStr,
			BodyEncoding: bodyEnc,
			BodyBytes:    len(full),
			Marker:       "break_glass_passthrough",
		})
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (h *LLMEndpointHandler) preprocessLiteSecretBody(w http.ResponseWriter, r *http.Request, agent *store.Agent, provider conversation.Provider, requestID string, body []byte, extraSuppressed map[string]struct{}, liteProxySecretDetectionDisabled bool, auditParams map[string]any, auditStatus *int, auditDecide, auditOutcome, auditReason *string) ([]byte, bool) {
	if stripped, err := llmproxy.StripSecretDecisionHistory(llmproxy.SecretDecisionHistoryStripRequest{
		Provider: provider,
		Body:     body,
	}); err != nil {
		h.emitLiteSecretPipelineTrace(requestID, agent, "history_strip_error", map[string]any{
			"provider": string(provider),
			"body_sha": liteSecretBodySHA(body),
			"err":      err.Error(),
		})
		h.Logger.WarnContext(r.Context(), "lite-proxy secret decision history strip failed",
			"agent_id", agent.ID, "err", err.Error())
	} else if stripped.Modified {
		h.emitLiteSecretPipelineTrace(requestID, agent, "history_stripped", map[string]any{
			"provider":        string(provider),
			"body_sha_before": liteSecretBodySHA(body),
			"body_sha_after":  liteSecretBodySHA(stripped.Body),
			"body_bytes":      len(stripped.Body),
		})
		body = stripped.Body
		auditParams["secret_decision_history_stripped"] = true
	}
	if liteProxySecretDetectionDisabled {
		h.emitLiteSecretPipelineTrace(requestID, agent, "lite_proxy_secret_detection_skipped", map[string]any{
			"provider": string(provider),
			"body_sha": liteSecretBodySHA(body),
			"reason":   "agent_setting",
		})
		return body, false
	}
	if rewritten, modified := h.applyRememberedSecretRewrites(r.Context(), agent, provider, requestID, body); modified {
		body = rewritten
		auditParams["secret_rewrites_applied"] = true
	}
	if h.maybeHoldInboundSecret(w, r, agent, provider, requestID, body, extraSuppressed, auditParams, auditStatus, auditDecide, auditOutcome, auditReason) {
		return body, true
	}
	return body, false
}

func agentLiteProxySecretDetectionDisabled(agent *store.Agent) bool {
	return agent != nil && (agent.RuntimeSettings == nil || agent.RuntimeSettings.LiteProxySecretDetectionDisabled)
}

// agentConversationAutoApproveThreshold reads the per-agent
// conversation-based auto-approval cap from the agent's runtime
// settings. Defaults to "off" when no runtime settings row exists or
// the agent itself is nil — matching the database column default so
// pre-feature agents keep the human-approval prompt.
func agentConversationAutoApproveThreshold(agent *store.Agent) string {
	if agent == nil || agent.RuntimeSettings == nil {
		return store.ConversationAutoApproveOff
	}
	if v := strings.TrimSpace(agent.RuntimeSettings.ConversationAutoApproveThreshold); v != "" {
		return v
	}
	return store.ConversationAutoApproveOff
}

func (h *LLMEndpointHandler) maybeHandleLiteSecretDecision(w http.ResponseWriter, r *http.Request, agent *store.Agent, provider conversation.Provider, requestID string, body []byte, auditParams map[string]any, auditStatus *int, auditDecide, auditOutcome, auditReason *string) ([]byte, llmproxy.SecretDecisionAction, map[string]struct{}, bool) {
	if h == nil || h.PendingSecrets == nil || agent == nil {
		return nil, "", nil, false
	}
	reply := llmproxy.SecretDecisionReplyFromBody(provider, body)
	if reply.Action == llmproxy.SecretDecisionNone {
		return nil, "", nil, false
	}
	// Prefer resolving by the specific decision ID embedded in the
	// preceding assistant prompt. Without this, a second pending
	// decision queued between the user being shown decision A and
	// replying would let "allow once" release A or B at random — and
	// can leak the wrong original body. Fall back to last-pending
	// only when the conversation history doesn't carry the marker
	// (e.g. corrupted client transport stripped it), which still
	// preserves the existing behavior for pre-existing flows.
	pendingID := llmproxy.LatestAssistantSecretDecisionID(provider, body)
	var (
		pending *llmproxy.PendingSecretDecision
		err     error
	)
	if pendingID != "" {
		pending, err = h.PendingSecrets.ResolveSecretID(r.Context(), agent.UserID, agent.ID, provider, pendingID)
	} else {
		pending, err = h.PendingSecrets.ResolveSecret(r.Context(), agent.UserID, agent.ID, provider)
	}
	if err != nil {
		h.Logger.WarnContext(r.Context(), "lite-proxy pending secret decision consume failed",
			"agent_id", agent.ID, "provider", string(provider), "err", err.Error())
		return nil, "", nil, false
	}
	if pending == nil {
		// Ambiguous: user typed a decision verb but we can't bind it
		// to a specific pending. Treat as no-op so the next pipeline
		// stage sees an ordinary turn.
		return nil, "", nil, false
	}
	if auditParams != nil {
		auditParams["secret_decision_id"] = pending.ID
		auditParams["secret_findings"] = len(pending.Findings)
	}
	h.emitLiteSecretPipelineTrace(requestID, agent, "decision_received", map[string]any{
		"provider":           string(provider),
		"decision":           string(reply.Action),
		"decision_id":        pending.ID,
		"pending_findings":   len(pending.Findings),
		"findings":           liteSecretFindingTraceSummaries(pending.Findings),
		"original_body_sha":  liteSecretBodySHA(pending.OriginalBody),
		"redacted_body_sha":  liteSecretBodySHA(pending.RedactedBody),
		"decision_body_sha":  liteSecretBodySHA(body),
		"decision_body_size": len(body),
	})
	switch reply.Action {
	case llmproxy.SecretDecisionAllowOnce:
		*auditStatus = 0
		h.emitLiteSecretPipelineTrace(requestID, agent, "decision_released", map[string]any{
			"provider":   string(provider),
			"decision":   string(reply.Action),
			"body_sha":   liteSecretBodySHA(pending.OriginalBody),
			"body_bytes": len(pending.OriginalBody),
		})
		return pending.OriginalBody, reply.Action, secretFindingFingerprintSet(pending.Findings), true
	case llmproxy.SecretDecisionNotSecret:
		h.rememberNotSecretFindings(r.Context(), agent, pending.Findings)
		h.emitLiteSecretPipelineTrace(requestID, agent, "decision_released", map[string]any{
			"provider":                string(provider),
			"decision":                string(reply.Action),
			"suppressed_fingerprints": liteSecretFindingFingerprintPrefixes(pending.Findings),
			"body_sha":                liteSecretBodySHA(pending.OriginalBody),
			"body_bytes":              len(pending.OriginalBody),
		})
		return pending.OriginalBody, reply.Action, nil, true
	case llmproxy.SecretDecisionDiscard:
		h.emitLiteSecretPipelineTrace(requestID, agent, "decision_released", map[string]any{
			"provider":   string(provider),
			"decision":   string(reply.Action),
			"body_sha":   liteSecretBodySHA(pending.RedactedBody),
			"body_bytes": len(pending.RedactedBody),
		})
		return pending.RedactedBody, reply.Action, nil, true
	case llmproxy.SecretDecisionVault:
		name := strings.TrimSpace(reply.VaultName)
		if name == "" && len(pending.Findings) > 0 {
			name = pending.Findings[0].SuggestedName
		}
		if name == "" {
			name = "secret"
		}
		vaulted := make([]liteSecretVaultedFinding, 0, len(pending.Findings))
		for i, finding := range pending.Findings {
			vaultName := name
			if len(pending.Findings) > 1 {
				vaultName = fmt.Sprintf("%s_%d", name, i+1)
			}
			placeholder, authID, vaultCreated, err := h.vaultFindingAndMintSessionPlaceholder(r.Context(), agent, vaultName, finding)
			if err != nil {
				h.rollbackPartialSecretVaults(r.Context(), agent, vaulted)
				h.requeuePendingSecretDecision(r.Context(), pending)
				if errors.Is(err, errSecretVaultNameConflict) {
					*auditStatus = http.StatusConflict
					*auditOutcome = "secret_vault_name_conflict"
					writeJSONError(w, http.StatusConflict, "SECRET_VAULT_NAME_CONFLICT", "vault item already exists with a different value; choose a different vault name")
				} else {
					*auditStatus = http.StatusInternalServerError
					*auditOutcome = "secret_vault_failed"
					writeJSONError(w, http.StatusInternalServerError, "SECRET_VAULT_FAILED", "could not save detected secret")
				}
				*auditDecide = "deny"
				*auditReason = err.Error()
				return nil, reply.Action, nil, true
			}
			vaulted = append(vaulted, liteSecretVaultedFinding{
				vaultName:       vaultName,
				resolvedVaultID: resolvedSecretVaultItemID(vaultName, finding),
				finding:         finding,
				placeholder:     placeholder,
				authID:          authID,
				vaultCreated:    vaultCreated,
			})
		}
		resumeBody := append([]byte{}, pending.OriginalBody...)
		for _, item := range vaulted {
			h.emitLiteSecretPipelineTrace(requestID, agent, "decision_vaulted_finding", map[string]any{
				"provider":           string(provider),
				"decision_id":        pending.ID,
				"vault_item_id":      item.resolvedVaultID,
				"finding":            liteSecretFindingTraceSummary(item.finding),
				"placeholder_prefix": liteSecretPlaceholderPrefix(item.placeholder),
			})
			rewrittenBody, modified, rewriteErr := rewriteJSONStrings(resumeBody, map[string]string{item.finding.Value: item.placeholder})
			if rewriteErr != nil || !modified {
				h.rollbackPartialSecretVaults(r.Context(), agent, vaulted)
				h.requeuePendingSecretDecision(r.Context(), pending)
				*auditStatus = http.StatusInternalServerError
				*auditDecide = "deny"
				*auditOutcome = "secret_vault_failed"
				if rewriteErr != nil {
					*auditReason = rewriteErr.Error()
				} else {
					*auditReason = "detected secret was not present in request JSON"
				}
				writeJSONError(w, http.StatusInternalServerError, "SECRET_VAULT_FAILED", "could not rewrite detected secret")
				return nil, reply.Action, nil, true
			}
			resumeBody = rewrittenBody
		}
		for _, item := range vaulted {
			h.rememberVaultedSecretRewrite(r.Context(), agent, item.vaultName, item.finding, item.placeholder)
		}
		h.emitLiteSecretPipelineTrace(requestID, agent, "decision_released", map[string]any{
			"provider":   string(provider),
			"decision":   string(reply.Action),
			"body_sha":   liteSecretBodySHA(resumeBody),
			"body_bytes": len(resumeBody),
		})
		return resumeBody, reply.Action, nil, true
	default:
		return nil, "", nil, false
	}
}

func (h *LLMEndpointHandler) requeuePendingSecretDecision(ctx context.Context, pending *llmproxy.PendingSecretDecision) {
	if h == nil || h.PendingSecrets == nil || pending == nil {
		return
	}
	if _, err := h.PendingSecrets.HoldSecret(ctx, *pending); err != nil {
		h.Logger.WarnContext(ctx, "lite-proxy pending secret decision requeue failed",
			"agent_id", pending.AgentID, "provider", string(pending.Provider), "decision_id", pending.ID, "err", err.Error())
	}
}

func (h *LLMEndpointHandler) vaultFindingAndMintSessionPlaceholder(ctx context.Context, agent *store.Agent, vaultName string, finding llmproxy.InboundSecretFinding) (string, string, bool, error) {
	if h == nil || h.Store == nil || h.Vault == nil || agent == nil {
		return "", "", false, fmt.Errorf("secret vault path is not configured")
	}
	vaultItemID := strings.TrimSpace(finding.ExistingVaultItemID)
	if vaultItemID == "" {
		vaultItemID = strings.TrimSpace(vaultName)
	}
	if vaultItemID == "" {
		vaultItemID = "secret"
	}
	created := liteSecretVaultedFinding{
		vaultName:       strings.TrimSpace(vaultName),
		resolvedVaultID: vaultItemID,
		finding:         finding,
	}
	rollback := func(err error) (string, string, bool, error) {
		h.rollbackPartialSecretVaults(ctx, agent, []liteSecretVaultedFinding{created})
		return "", "", false, err
	}
	if finding.ExistingVaultItemID == "" {
		existing, err := h.Vault.Get(ctx, agent.UserID, vaultItemID)
		switch {
		case err == nil && !bytes.Equal(existing, []byte(finding.Value)):
			return "", "", false, errSecretVaultNameConflict
		case err == nil:
			// Reuse the existing entry when the user picked the same vault
			// name for the same value. Do not mark it for rollback.
		case errors.Is(err, vault.ErrNotFound):
			if err := h.Vault.Set(ctx, agent.UserID, vaultItemID, []byte(finding.Value)); err != nil {
				return "", "", false, err
			}
			created.vaultCreated = true
		default:
			return "", "", false, err
		}
	}
	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	if agent.TokenExpiresAt != nil && agent.TokenExpiresAt.Before(expiresAt) {
		expiresAt = agent.TokenExpiresAt.UTC()
	}
	auth := &store.CredentialAuthorization{
		ID:            uuid.NewString(),
		UserID:        agent.UserID,
		AgentID:       agent.ID,
		Scope:         "session",
		CredentialRef: vaultItemID,
		Service:       vaultItemID,
		Host:          "",
		HeaderName:    "authorization",
		Scheme:        "bearer",
		Status:        "active",
		MetadataJSON: mustJSON(map[string]any{
			"source":        "lite_proxy_secret_detection",
			"vault_item_id": vaultItemID,
			"decision":      "vault",
		}),
		ExpiresAt: &expiresAt,
	}
	if err := h.Store.CreateCredentialAuthorization(ctx, auth); err != nil {
		return rollback(err)
	}
	created.authID = auth.ID
	placeholder, err := runtimeautovault.GeneratePlaceholder(runtimeautovault.PlaceholderPrefix(vaultItemID))
	if err != nil {
		return rollback(err)
	}
	created.placeholder = placeholder
	if err := h.Store.CreateRuntimePlaceholder(ctx, &store.RuntimePlaceholder{
		Placeholder:       placeholder,
		UserID:            agent.UserID,
		AgentID:           agent.ID,
		ServiceID:         vaultItemID,
		VaultItemID:       vaultItemID,
		CredentialGrantID: auth.ID,
		ExpiresAt:         &expiresAt,
	}); err != nil {
		return rollback(err)
	}
	return placeholder, auth.ID, created.vaultCreated, nil
}

type liteSecretVaultRewrite struct {
	Placeholder string
}

type liteSecretVaultedFinding struct {
	vaultName       string
	resolvedVaultID string
	finding         llmproxy.InboundSecretFinding
	placeholder     string
	authID          string
	vaultCreated    bool
}

func resolvedSecretVaultItemID(vaultName string, finding llmproxy.InboundSecretFinding) string {
	if strings.TrimSpace(finding.ExistingVaultItemID) != "" {
		return strings.TrimSpace(finding.ExistingVaultItemID)
	}
	vaultName = strings.TrimSpace(vaultName)
	if vaultName == "" {
		return "secret"
	}
	return vaultName
}

func (h *LLMEndpointHandler) rollbackPartialSecretVaults(ctx context.Context, agent *store.Agent, vaulted []liteSecretVaultedFinding) {
	if h == nil || agent == nil {
		return
	}
	for _, item := range vaulted {
		if h.Store != nil && strings.TrimSpace(item.placeholder) != "" {
			if err := h.Store.DeleteRuntimePlaceholder(ctx, item.placeholder, agent.UserID); err != nil && !errors.Is(err, store.ErrNotFound) {
				h.Logger.WarnContext(ctx, "lite-proxy partial secret placeholder rollback failed",
					"agent_id", agent.ID, "placeholder_prefix", liteSecretPlaceholderPrefix(item.placeholder), "err", err.Error())
			}
		}
		if h.Store != nil && strings.TrimSpace(item.authID) != "" {
			if err := h.Store.DeleteCredentialAuthorization(ctx, item.authID, agent.UserID); err != nil && !errors.Is(err, store.ErrNotFound) {
				h.Logger.WarnContext(ctx, "lite-proxy partial secret credential authorization rollback failed",
					"agent_id", agent.ID, "auth_id", item.authID, "err", err.Error())
			}
		}
		if h.Vault != nil && item.vaultCreated && strings.TrimSpace(item.resolvedVaultID) != "" {
			if err := h.Vault.Delete(ctx, agent.UserID, item.resolvedVaultID); err != nil && !errors.Is(err, vault.ErrNotFound) {
				h.Logger.WarnContext(ctx, "lite-proxy partial secret vault rollback failed",
					"agent_id", agent.ID, "vault_item_id", item.resolvedVaultID, "err", err.Error())
			}
		}
	}
}

func (h *LLMEndpointHandler) applyRememberedSecretRewrites(ctx context.Context, agent *store.Agent, provider conversation.Provider, requestID string, body []byte) ([]byte, bool) {
	if h == nil || h.Store == nil || agent == nil || len(body) == 0 {
		return body, false
	}
	rewrites := h.loadActiveSecretRewrites(ctx, agent)
	if len(rewrites) == 0 {
		h.emitLiteSecretPipelineTrace(requestID, agent, "rewrite_scan_skipped", map[string]any{
			"provider": string(provider),
			"reason":   "no_active_rewrites",
			"body_sha": liteSecretBodySHA(body),
		})
		return body, false
	}
	scan, found, err := llmproxy.ScanInboundSecretsWithOptions(ctx, llmproxy.InboundSecretScanOptions{
		Provider: provider,
		Host:     string(provider),
		Body:     body,
	})
	if err != nil {
		h.emitLiteSecretPipelineTrace(requestID, agent, "rewrite_scan_error", map[string]any{
			"provider":      string(provider),
			"active_rules":  len(rewrites),
			"body_sha":      liteSecretBodySHA(body),
			"error_message": err.Error(),
		})
		return body, false
	}
	if !found {
		h.emitLiteSecretPipelineTrace(requestID, agent, "rewrite_scan_no_findings", map[string]any{
			"provider":     string(provider),
			"active_rules": len(rewrites),
			"body_sha":     liteSecretBodySHA(body),
		})
		return body, false
	}
	out := append([]byte{}, body...)
	modified := false
	matched := 0
	replacements := map[string]string{}
	for _, finding := range scan.Findings {
		rewrite, ok := rewrites[finding.Fingerprint]
		if !ok || rewrite.Placeholder == "" || finding.Value == "" {
			continue
		}
		matched++
		replacements[finding.Value] = rewrite.Placeholder
	}
	if len(replacements) > 0 {
		rewritten, rewriteModified, rewriteErr := rewriteJSONStrings(out, replacements)
		if rewriteErr != nil {
			h.emitLiteSecretPipelineTrace(requestID, agent, "rewrite_scan_error", map[string]any{
				"provider":      string(provider),
				"active_rules":  len(rewrites),
				"body_sha":      liteSecretBodySHA(body),
				"error_message": rewriteErr.Error(),
			})
			return body, false
		}
		out = rewritten
		modified = rewriteModified
	}
	h.emitLiteSecretPipelineTrace(requestID, agent, "rewrite_scan_done", map[string]any{
		"provider":        string(provider),
		"adjudications":   liteSecretAdjudicationTraceSummaries(scan.Adjudications),
		"active_rules":    len(rewrites),
		"findings":        liteSecretFindingTraceSummaries(scan.Findings),
		"findings_count":  len(scan.Findings),
		"matched_count":   matched,
		"modified":        modified,
		"body_sha_before": liteSecretBodySHA(body),
		"body_sha_after":  liteSecretBodySHA(out),
	})
	return out, modified
}

func rewriteJSONStrings(body []byte, replacements map[string]string) ([]byte, bool, error) {
	if len(body) == 0 || len(replacements) == 0 {
		return body, false, nil
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false, err
	}
	keys := sortedReplacementKeys(replacements)
	rewritten, modified := rewriteJSONValueStrings(parsed, replacements, keys)
	if !modified {
		return body, false, nil
	}
	// Re-marshaling intentionally canonicalizes object key order and
	// whitespace. The upstream LLM APIs only care about semantic JSON.
	out, err := json.Marshal(rewritten)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func sortedReplacementKeys(replacements map[string]string) []string {
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		if key == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) == len(keys[j]) {
			return keys[i] < keys[j]
		}
		return len(keys[i]) > len(keys[j])
	})
	return keys
}

func rewriteJSONValueStrings(value any, replacements map[string]string, keys []string) (any, bool) {
	switch typed := value.(type) {
	case string:
		out := typed
		for _, secret := range keys {
			out = strings.ReplaceAll(out, secret, replacements[secret])
		}
		return out, out != typed
	case []any:
		modified := false
		for i, item := range typed {
			rewritten, changed := rewriteJSONValueStrings(item, replacements, keys)
			if changed {
				typed[i] = rewritten
				modified = true
			}
		}
		return typed, modified
	case map[string]any:
		modified := false
		for key, item := range typed {
			rewritten, changed := rewriteJSONValueStrings(item, replacements, keys)
			if changed {
				typed[key] = rewritten
				modified = true
			}
		}
		return typed, modified
	default:
		return value, false
	}
}

func (h *LLMEndpointHandler) loadActiveSecretRewrites(ctx context.Context, agent *store.Agent) map[string]liteSecretVaultRewrite {
	out := map[string]liteSecretVaultRewrite{}
	if h == nil || h.Store == nil || agent == nil {
		return out
	}
	enabled := true
	rules, err := h.Store.ListRuntimePolicyRules(ctx, agent.UserID, store.RuntimePolicyRuleFilter{
		AgentID: agent.ID,
		Kind:    "secret_rewrite",
		Enabled: &enabled,
	})
	if err != nil {
		return out
	}
	now := time.Now().UTC()
	for _, rule := range rules {
		if rule == nil || rule.Action != "replace" || strings.TrimSpace(rule.Host) == "" || strings.TrimSpace(rule.Path) == "" {
			continue
		}
		placeholder, err := h.Store.GetRuntimePlaceholder(ctx, strings.TrimSpace(rule.Path))
		if err != nil {
			continue
		}
		if _, ok := llmproxy.ValidateRuntimePlaceholderAccess(ctx, h.Store, placeholder, agent.UserID, agent.ID, now); !ok {
			continue
		}
		vaultItemID := strings.TrimSpace(placeholder.VaultItemID)
		if vaultItemID == "" {
			vaultItemID = strings.TrimSpace(placeholder.ServiceID)
		}
		if h.Vault != nil && vaultItemID != "" {
			if _, err := h.Vault.Get(ctx, agent.UserID, vaultItemID); err != nil {
				continue
			}
		}
		out[strings.TrimSpace(rule.Host)] = liteSecretVaultRewrite{
			Placeholder: strings.TrimSpace(rule.Path),
		}
	}
	return out
}

func (h *LLMEndpointHandler) maybeHoldInboundSecret(w http.ResponseWriter, r *http.Request, agent *store.Agent, provider conversation.Provider, requestID string, body []byte, extraSuppressed map[string]struct{}, auditParams map[string]any, auditStatus *int, auditDecide, auditOutcome, auditReason *string) bool {
	if h == nil || h.PendingSecrets == nil || agent == nil {
		return false
	}
	suppressed := h.secretSuppressionFingerprints(r.Context(), agent)
	for fp := range extraSuppressed {
		suppressed[fp] = struct{}{}
	}
	h.emitLiteSecretPipelineTrace(requestID, agent, "hold_scan_start", map[string]any{
		"provider":         string(provider),
		"body_sha":         liteSecretBodySHA(body),
		"body_bytes":       len(body),
		"suppressed_count": len(suppressed),
	})
	scan, found, err := llmproxy.ScanInboundSecretsWithOptions(r.Context(), llmproxy.InboundSecretScanOptions{
		Provider:    provider,
		Host:        string(provider),
		Body:        body,
		Suppressed:  suppressed,
		Adjudicator: h.SecretAdjudicator,
	})
	if err != nil {
		h.emitLiteSecretPipelineTrace(requestID, agent, "hold_scan_error", map[string]any{
			"provider":      string(provider),
			"body_sha":      liteSecretBodySHA(body),
			"error_message": err.Error(),
		})
		h.Logger.WarnContext(r.Context(), "lite-proxy inbound secret scan failed",
			"request_id", requestID,
			"agent_id", agent.ID,
			"err", err.Error())
		return false
	}
	if !found {
		h.emitLiteSecretPipelineTrace(requestID, agent, "hold_scan_no_findings", map[string]any{
			"provider":      string(provider),
			"adjudications": liteSecretAdjudicationTraceSummaries(scan.Adjudications),
			"body_sha":      liteSecretBodySHA(body),
		})
		return false
	}
	scan.Findings = h.annotateExistingVaultSecrets(r.Context(), agent.UserID, scan.Findings)
	h.emitLiteSecretPipelineTrace(requestID, agent, "hold_scan_findings", map[string]any{
		"provider":          string(provider),
		"adjudications":     liteSecretAdjudicationTraceSummaries(scan.Adjudications),
		"findings_count":    len(scan.Findings),
		"findings":          liteSecretFindingTraceSummaries(scan.Findings),
		"body_sha_before":   liteSecretBodySHA(body),
		"redacted_body_sha": liteSecretBodySHA(scan.RedactedBody),
	})
	held, err := h.PendingSecrets.HoldSecret(r.Context(), llmproxy.PendingSecretDecision{
		UserID:       agent.UserID,
		AgentID:      agent.ID,
		Provider:     provider,
		OriginalBody: append([]byte{}, body...),
		RedactedBody: append([]byte{}, scan.RedactedBody...),
		Findings:     scan.Findings,
	})
	if err != nil {
		h.emitLiteSecretPipelineTrace(requestID, agent, "hold_failed", map[string]any{
			"provider":      string(provider),
			"body_sha":      liteSecretBodySHA(body),
			"error_message": err.Error(),
		})
		*auditStatus = http.StatusInternalServerError
		*auditDecide = "deny"
		*auditOutcome = "secret_hold_failed"
		*auditReason = err.Error()
		writeJSONError(w, http.StatusInternalServerError, "SECRET_HOLD_FAILED", "could not hold detected secret")
		return true
	}
	if auditParams != nil {
		auditParams["secret_decision_id"] = held.ID
		auditParams["secret_findings"] = len(scan.Findings)
		auditParams["secret_suggested_name"] = scan.Findings[0].SuggestedName
		auditParams["secret_sources"] = liteSecretFindingSources(scan.Findings)
	}
	h.emitLiteSecretPipelineTrace(requestID, agent, "hold_created", map[string]any{
		"provider":          string(provider),
		"adjudications":     liteSecretAdjudicationTraceSummaries(scan.Adjudications),
		"decision_id":       held.ID,
		"findings_count":    len(scan.Findings),
		"findings":          liteSecretFindingTraceSummaries(scan.Findings),
		"redacted_body_sha": liteSecretBodySHA(scan.RedactedBody),
		"expires_at":        held.ExpiresAt.Format(time.RFC3339Nano),
	})
	if h.RawIOLogger != nil {
		bodyStr, bodyEnc := llmproxy.EncodeBody(scan.RedactedBody)
		h.RawIOLogger.Emit(llmproxy.RawIOEvent{
			Phase:        "inbound_secret_hold",
			RequestID:    requestID,
			UserID:       agent.UserID,
			AgentID:      agent.ID,
			Provider:     string(provider),
			Method:       r.Method,
			Path:         r.URL.RequestURI(),
			Headers:      llmproxy.SafeHeaderSnapshot(r.Header),
			Body:         bodyStr,
			BodyEncoding: bodyEnc,
			BodyBytes:    len(scan.RedactedBody),
			Marker:       "redacted_pre_hold",
		})
	}
	*auditStatus = http.StatusOK
	*auditDecide = "block"
	*auditOutcome = "secret_detected"
	*auditReason = "raw secret detected in inbound LLM request"
	prompt := renderInboundSecretPrompt(held)
	bodyBytes, contentType := syntheticLiteTextResponse(r, provider, body, prompt)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bodyBytes)
	return true
}

func (h *LLMEndpointHandler) annotateExistingVaultSecrets(ctx context.Context, userID string, findings []llmproxy.InboundSecretFinding) []llmproxy.InboundSecretFinding {
	if h == nil || h.Vault == nil || len(findings) == 0 {
		return findings
	}
	keys, err := h.Vault.List(ctx, userID)
	if err != nil || len(keys) == 0 {
		return findings
	}
	byValue := make(map[string]string, len(findings))
	for _, key := range keys {
		raw, err := h.Vault.Get(ctx, userID, key)
		if err != nil || len(raw) == 0 {
			continue
		}
		if _, exists := byValue[string(raw)]; !exists {
			byValue[string(raw)] = key
		}
	}
	out := append([]llmproxy.InboundSecretFinding(nil), findings...)
	for i := range out {
		if existing := byValue[out[i].Value]; existing != "" {
			out[i].ExistingVaultItemID = existing
			if out[i].SuggestedName == "" || out[i].SuggestedName == "secret" {
				out[i].SuggestedName = existing
			}
		}
	}
	return out
}

func (h *LLMEndpointHandler) secretSuppressionFingerprints(ctx context.Context, agent *store.Agent) map[string]struct{} {
	out := map[string]struct{}{}
	if h == nil || h.Store == nil || agent == nil {
		return out
	}
	enabled := true
	rules, err := h.Store.ListRuntimePolicyRules(ctx, agent.UserID, store.RuntimePolicyRuleFilter{
		AgentID: agent.ID,
		Kind:    "secret_suppression",
		Enabled: &enabled,
	})
	if err != nil {
		return out
	}
	for _, rule := range rules {
		if rule == nil || rule.Action != "allow" || strings.TrimSpace(rule.Host) == "" {
			continue
		}
		out[strings.TrimSpace(rule.Host)] = struct{}{}
	}
	return out
}

func (h *LLMEndpointHandler) rememberNotSecretFindings(ctx context.Context, agent *store.Agent, findings []llmproxy.InboundSecretFinding) {
	if h == nil || h.Store == nil || agent == nil {
		return
	}
	for _, finding := range findings {
		if strings.TrimSpace(finding.Fingerprint) == "" {
			continue
		}
		rule := &store.RuntimePolicyRule{
			ID:       uuid.NewString(),
			UserID:   agent.UserID,
			AgentID:  &agent.ID,
			Kind:     "secret_suppression",
			Action:   "allow",
			Host:     finding.Fingerprint,
			Reason:   "user marked detected value as not a secret",
			Source:   "secret_detection",
			Enabled:  true,
			ToolName: finding.SuggestedName,
		}
		if err := h.Store.CreateRuntimePolicyRule(ctx, rule); err != nil && !errors.Is(err, store.ErrConflict) {
			h.Logger.WarnContext(ctx, "lite-proxy secret suppression save failed",
				"agent_id", agent.ID, "fingerprint", finding.Fingerprint, "err", err.Error())
		}
	}
}

func (h *LLMEndpointHandler) rememberVaultedSecretRewrite(ctx context.Context, agent *store.Agent, vaultItemID string, finding llmproxy.InboundSecretFinding, placeholder string) {
	if h == nil || h.Store == nil || agent == nil {
		return
	}
	if strings.TrimSpace(finding.Fingerprint) == "" || strings.TrimSpace(placeholder) == "" {
		return
	}
	if finding.ExistingVaultItemID != "" {
		vaultItemID = finding.ExistingVaultItemID
	}
	rule := &store.RuntimePolicyRule{
		ID:       uuid.NewString(),
		UserID:   agent.UserID,
		AgentID:  &agent.ID,
		Kind:     "secret_rewrite",
		Action:   "replace",
		Service:  strings.TrimSpace(vaultItemID),
		Host:     finding.Fingerprint,
		Path:     placeholder,
		Reason:   "user vaulted detected secret; replay stable placeholder in later transcript history",
		Source:   "secret_detection",
		Enabled:  true,
		ToolName: finding.SuggestedName,
	}
	if err := h.Store.CreateRuntimePolicyRule(ctx, rule); err != nil && !errors.Is(err, store.ErrConflict) {
		h.Logger.WarnContext(ctx, "lite-proxy secret rewrite save failed",
			"agent_id", agent.ID, "fingerprint", finding.Fingerprint, "err", err.Error())
	}
}

func (h *LLMEndpointHandler) emitLiteSecretPipelineTrace(requestID string, agent *store.Agent, stage string, fields map[string]any) {
	if h == nil || h.TraceLogger == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["event"] = llmproxy.TraceEventSecretPipeline
	fields["stage"] = stage
	if requestID != "" {
		fields["request_id"] = requestID
	}
	if agent != nil {
		fields["agent_id"] = agent.ID
		fields["user_id"] = agent.UserID
	}
	h.TraceLogger.Emit(fields)
}

// liteSecretBodySHA produces a 16-char fingerprint of the request body
// used to correlate "this body was held → this body was released" entries
// in the audit / trace stream. It is NOT a credential hash and not
// compared in any security-sensitive way; the truncation to 64 bits of
// hex tells the same story. SHA-256 is the right primitive for a
// content fingerprint and lgtm/CodeQL's weak-hash heuristic fires on
// the function name only.
func liteSecretBodySHA(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// codeql[go/weak-cryptographic-algorithm] Request body hash is a non-secret cache key, not a security boundary.
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%x", sum[:])[:16]
}

func liteSecretFindingTraceSummaries(findings []llmproxy.InboundSecretFinding) []map[string]any {
	out := make([]map[string]any, 0, len(findings))
	for _, finding := range findings {
		out = append(out, liteSecretFindingTraceSummary(finding))
	}
	return out
}

func liteSecretAdjudicationTraceSummaries(adjudications []llmproxy.InboundSecretAdjudication) []map[string]any {
	out := make([]map[string]any, 0, len(adjudications))
	for _, adjudication := range adjudications {
		summary := map[string]any{
			"fingerprint_prefix": liteSecretFingerprintPrefix(adjudication.Fingerprint),
			"outcome":            adjudication.Outcome,
		}
		if adjudication.FieldName != "" {
			summary["field_name"] = adjudication.FieldName
		}
		if adjudication.Charset != "" {
			summary["charset"] = adjudication.Charset
		}
		if adjudication.Entropy > 0 {
			summary["entropy"] = adjudication.Entropy
		}
		if adjudication.DurationMS > 0 {
			summary["duration_ms"] = adjudication.DurationMS
		}
		if adjudication.Outcome == "verdict" {
			summary["credential"] = adjudication.Credential
			summary["confidence"] = adjudication.Confidence
			if adjudication.Service != "" {
				summary["service"] = adjudication.Service
			}
		}
		if adjudication.ErrorKind != "" {
			summary["error_kind"] = adjudication.ErrorKind
		}
		if adjudication.ErrorMessage != "" {
			summary["error_message"] = truncateLiteSecretTraceString(adjudication.ErrorMessage, 500)
		}
		out = append(out, summary)
	}
	return out
}

func liteSecretFindingTraceSummary(finding llmproxy.InboundSecretFinding) map[string]any {
	out := map[string]any{
		"fingerprint_prefix": liteSecretFingerprintPrefix(finding.Fingerprint),
		"source":             finding.Source,
		"service":            finding.Service,
		"suggested_name":     finding.SuggestedName,
	}
	if finding.ExistingVaultItemID != "" {
		out["existing_vault_item_id"] = finding.ExistingVaultItemID
	}
	if finding.Entropy > 0 {
		out["entropy"] = finding.Entropy
	}
	return out
}

func truncateLiteSecretTraceString(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	if max <= 3 {
		return value[:max]
	}
	return value[:max-3] + "..."
}

func liteSecretFindingFingerprintPrefixes(findings []llmproxy.InboundSecretFinding) []string {
	out := make([]string, 0, len(findings))
	for _, finding := range findings {
		if prefix := liteSecretFingerprintPrefix(finding.Fingerprint); prefix != "" {
			out = append(out, prefix)
		}
	}
	return out
}

func secretFindingFingerprintSet(findings []llmproxy.InboundSecretFinding) map[string]struct{} {
	out := make(map[string]struct{}, len(findings))
	for _, finding := range findings {
		if strings.TrimSpace(finding.Fingerprint) == "" {
			continue
		}
		out[finding.Fingerprint] = struct{}{}
	}
	return out
}

func liteSecretFindingSources(findings []llmproxy.InboundSecretFinding) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, finding := range findings {
		source := strings.TrimSpace(finding.Source)
		if source == "" {
			continue
		}
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		out = append(out, source)
	}
	return out
}

func liteSecretFingerprintPrefix(fingerprint string) string {
	fingerprint = strings.TrimSpace(fingerprint)
	if len(fingerprint) <= 12 {
		return fingerprint
	}
	return fingerprint[:12]
}

func liteSecretPlaceholderPrefix(placeholder string) string {
	placeholder = strings.TrimSpace(placeholder)
	if placeholder == "" {
		return ""
	}
	if len(placeholder) <= 24 {
		return placeholder + "..."
	}
	return placeholder[:24] + "..."
}

func renderInboundSecretPrompt(pending llmproxy.PendingSecretDecision) string {
	name := "secret"
	source := "heuristic"
	if len(pending.Findings) > 0 {
		name = promptSafeSecretToken(pending.Findings[0].SuggestedName)
		source = promptSafeSecretToken(pending.Findings[0].Source)
	}
	if len(pending.Findings) > 0 && pending.Findings[0].ExistingVaultItemID != "" {
		existing := promptSafeSecretToken(pending.Findings[0].ExistingVaultItemID)
		commandName := promptSafeSecretToken(pending.Findings[0].ExistingVaultItemID)
		return fmt.Sprintf(llmproxy.SecretDecisionPromptMarker+" in the last message.\n\nThis value already exists in the vault as `%s`, so choosing vault will reuse that entry instead of creating a duplicate.\nDetection source: %s\n\nReply `vault %s` to continue with a redacted message, `discard` to continue with it redacted without changing the vault, `allow once` to send it this time without vaulting, or `not secret` to remember that this value is not a secret.\n\n%s%s]", existing, source, commandName, llmproxy.SecretDecisionIDMarker, pending.ID)
	}
	return fmt.Sprintf(llmproxy.SecretDecisionPromptMarker+" in the last message.\n\nSuggested vault name: `%s`\nDetection source: %s\n\nReply `vault %s` to save it and continue with a redacted message, `discard` to continue with it redacted, `allow once` to send it this time without vaulting, or `not secret` to remember that this value is not a secret.\n\n%s%s]", name, source, name, llmproxy.SecretDecisionIDMarker, pending.ID)
}

func promptSafeSecretToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "secret"
	}
	replacer := strings.NewReplacer("`", "_", "\n", "_", "\r", "_", "\t", "_")
	value = replacer.Replace(value)
	if len(value) > 96 {
		value = value[:96]
	}
	return value
}

func syntheticLiteTextResponse(r *http.Request, provider conversation.Provider, requestBody []byte, text string) ([]byte, string) {
	stream := liteProxyRequestDebugSummary(provider, requestBody).Stream
	switch provider {
	case conversation.ProviderAnthropic:
		if stream {
			return conversation.SynthAnthropicTextSSE("", "", "assistant", text), "text/event-stream"
		}
		return conversation.SynthAnthropicTextJSON("", "", "assistant", text), "application/json"
	case conversation.ProviderOpenAI:
		if conversation.IsOpenAIChatCompletionsEndpoint(r) {
			if stream {
				return conversation.SynthOpenAIChatTextSSE(text), "text/event-stream"
			}
			return conversation.SynthOpenAIChatTextJSON(text), "application/json"
		}
		if stream {
			return conversation.SynthOpenAIResponsesTextSSE(text), "text/event-stream"
		}
		return conversation.SynthOpenAIResponsesTextJSON(text), "application/json"
	default:
		raw, _ := json.Marshal(map[string]string{"message": text})
		return raw, "application/json"
	}
}

func (h *LLMEndpointHandler) maybeHandleLiteApprovalRelease(w http.ResponseWriter, r *http.Request, agent *store.Agent, provider conversation.Provider, requestID, conversationID string, body []byte, auditStatus *int, auditDecide, auditOutcome, auditReason *string) bool {
	candidateTasks, toolRules, egressRules, decisionLoadErr := h.loadLiteProxyDecisionInputs(r.Context(), agent)
	if decisionLoadErr != nil {
		// Approval-release path also authorizes; same fail-closed rule
		// as the main serve() path applies.
		h.Logger.WarnContext(r.Context(), "lite-proxy approval-release decision-input load failed; failing closed",
			"request_id", requestID, "agent_id", agent.ID, "err", decisionLoadErr.Error())
		*auditStatus = http.StatusServiceUnavailable
		*auditDecide = "deny"
		*auditOutcome = "decision_input_load_failed"
		*auditReason = decisionLoadErr.Error()
		clearMirroredUpstreamHeaders(w.Header())
		writeJSONError(w, http.StatusServiceUnavailable, "DECISION_INPUT_UNAVAILABLE",
			"authorization inputs unavailable; please retry")
		return true
	}
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
		ConversationID:  conversationID,
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
	hasClawvisorAgentToken := strings.TrimSpace(r.Header.Get(middleware.AgentTokenHeader)) != ""
	switch {
	case hasClawvisorAgentToken && hasBearer && hasAPIKey:
		return "clawvisor-agent-token+authorization+x-api-key"
	case hasClawvisorAgentToken && hasBearer:
		return "clawvisor-agent-token+authorization"
	case hasClawvisorAgentToken && hasAPIKey:
		return "clawvisor-agent-token+x-api-key"
	case hasClawvisorAgentToken:
		return "clawvisor-agent-token"
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
// Clawvisor agent header, Authorization, or x-api-key header. Used as a
// fallback to source the caller token for the rewriter when no dedicated
// middleware ran.
func inboundAgentToken(r *http.Request) string {
	if h := clawvisorAgentTokenHeader(r); strings.HasPrefix(h, "cvis_") {
		return h
	}
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

func clawvisorAgentTokenHeader(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get(middleware.AgentTokenHeader))
	if v == "" {
		return ""
	}
	const prefix = "Bearer "
	if strings.HasPrefix(v, prefix) {
		return strings.TrimSpace(v[len(prefix):])
	}
	return v
}

// taskRiskBridge adapts the handler's taskrisk.Assessor (which speaks the
// shared AssessRequest shape used by the dashboard task-create path) to
// the narrow llmproxy.TaskRiskAssessor interface the postprocess pipeline
// consumes. Returns nil when the handler has no assessor configured so
// the intercept's nil-check correctly falls back to the deterministic
// envelope policy.
func (h *LLMEndpointHandler) taskRiskBridge() llmproxy.TaskRiskAssessor {
	if h == nil || h.TaskRiskAssessor == nil {
		return nil
	}
	return &liteProxyTaskRiskBridge{assessor: h.TaskRiskAssessor}
}

type liteProxyTaskRiskBridge struct {
	assessor taskrisk.Assessor
}

func (b *liteProxyTaskRiskBridge) AssessEnvelope(ctx context.Context, req llmproxy.TaskRiskAssessRequest) *llmproxy.TaskRiskAssessment {
	if b == nil || b.assessor == nil {
		return nil
	}
	out, err := b.assessor.Assess(ctx, taskrisk.AssessRequest{
		Purpose:                req.Purpose,
		AgentName:              req.AgentName,
		UserID:                 req.UserID,
		ExpectedTools:          req.ExpectedTools,
		ExpectedEgress:         req.ExpectedEgress,
		RequiredCredentials:    req.RequiredCredentials,
		IntentVerificationMode: req.IntentVerificationMode,
		ExpectedUse:            req.ExpectedUse,
		RecentUserTurns:        req.RecentUserTurns,
	})
	if err != nil || out == nil {
		return nil
	}
	conflicts := make([]llmproxy.TaskRiskConflict, 0, len(out.Conflicts))
	for _, c := range out.Conflicts {
		conflicts = append(conflicts, llmproxy.TaskRiskConflict{
			Field:       c.Field,
			Description: c.Description,
			Severity:    c.Severity,
		})
	}
	return &llmproxy.TaskRiskAssessment{
		RiskLevel:              out.RiskLevel,
		Explanation:            out.Explanation,
		Factors:                out.Factors,
		Conflicts:              conflicts,
		IntentMatch:            out.IntentMatch,
		IntentMatchExplanation: out.IntentMatchExplanation,
	}
}
