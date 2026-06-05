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
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pricing"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
	"github.com/clawvisor/clawvisor/pkg/version"
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

	// DashboardBaseURL is the externally reachable dashboard host used
	// to deep-link the user from "no upstream key" errors to the page
	// where they can paste one. Should NOT include a trailing slash.
	// In monolithic / local deploys it equals ControlBaseURL; in
	// split-mode hosted deploys (route_set: proxy_lite) it must be set
	// separately because ControlBaseURL there is the proxy host, not
	// the dashboard. Empty falls back to the build-environment default
	// from pkg/version (correct for hosted builds, wrong for local).
	DashboardBaseURL string

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

	// DefaultTaskExpirySeconds mirrors the daemon's resolved
	// cfg.Task.DefaultExpirySeconds. Surfaced into the inline task
	// approval prompt so the displayed Duration matches the operator's
	// configured default when the agent omits expires_in_seconds.
	// Optional — 0 lets the renderer fall back to its 30-minute
	// constant, which is fine for tests and unconfigured deploys but
	// would drift from the resolved scope in production.
	DefaultTaskExpirySeconds int

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
		// Cost-extraction state populated once the upstream response
		// has been buffered. The deferred audit emission below reads
		// these to write a paired llm_request_cost row.
		auditUsage  *llmproxy.ExtractUsageResult
		auditTaskID string
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
			"caller_auth_source", llmproxy.CallerAuthSource(r.Context()),
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
			time.Since(start), auditParams,
			llmproxy.EndpointCallExtras{TaskID: auditTaskID, Usage: auditUsage})
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
	if src := llmproxy.CallerAuthSource(r.Context()); src != "" {
		auditParams["caller_auth_source"] = src
	}
	if llmproxy.PassthroughUpstreamAuth(r.Context()) {
		auditParams["upstream_auth_passthrough_requested"] = true
		auditParams["upstream_auth_passthrough_bearer_present"] = llmproxy.HasPassthroughBearer(r)
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
		h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "this request is too large to process. Please shorten it and try again.")
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
	//
	// First migrated call site through internal/runtime/llmproxy/pipeline:
	// runs the anthropic_sanitize policy via Pipeline.RunPre. Other
	// migrated policies (inbound_sanitize, control_notice, etc.) still
	// run inline below; full consolidation arrives once all preprocess
	// policies migrate.
	{
		pipeReq := &pipelineReadOnlyRequest{
			provider: provider,
			httpReq:  r,
			body:     body,
			userID:   agent.UserID,
			agentID:  agent.ID,
		}
		result, err := runSinglePolicy(r.Context(), pipeReq, policies.NewAnthropicSanitize())
		if err != nil {
			auditStatus = http.StatusInternalServerError
			auditDecide = "deny"
			auditOutcome = "pipeline_error"
			auditReason = err.Error()
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusInternalServerError, "PIPELINE_ERROR", "internal pipeline error: "+err.Error()+". Please retry.")
			return
		}
		if result.DenyReason != "" {
			auditStatus = http.StatusBadRequest
			auditDecide = "deny"
			auditOutcome = "malformed_request"
			auditReason = result.DenyReason
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadRequest, "MALFORMED_REQUEST", "couldn't parse this request: "+result.DenyReason+". This usually means a client bug; please retry.")
			return
		}
		body = result.FinalBody
		for k, v := range result.AuditFields {
			auditParams[k] = v
		}
	}
	if _, err := parser.ParseRequest(body); err != nil {
		auditStatus = http.StatusBadRequest
		auditDecide = "deny"
		auditOutcome = "malformed_request"
		auditReason = err.Error()
		h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadRequest, "MALFORMED_REQUEST", "couldn't parse this request: "+err.Error()+". This usually means a client bug; please retry.")
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
				h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadRequest, "MALFORMED_REQUEST", "couldn't parse this request: "+err.Error()+". This usually means a client bug; please retry.")
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
	//
	// Second migrated call site through pipeline.
	{
		pipeReq := &pipelineReadOnlyRequest{
			provider: provider,
			httpReq:  r,
			body:     body,
			userID:   agent.UserID,
			agentID:  agent.ID,
		}
		result, err := runSinglePolicy(r.Context(), pipeReq, policies.NewInboundSanitize(h.ResolverBaseURL, h.ControlBaseURL))
		if err != nil {
			// inbound_sanitize uses best-effort semantics today —
			// a pipeline failure here was a hard failure (panic on
			// PanicMutator method). Log and continue with the
			// un-sanitized body so the request still works.
			h.Logger.WarnContext(r.Context(), "lite-proxy inbound sanitize pipeline failed",
				"agent_id", agent.ID, "err", err.Error())
		} else {
			body = result.FinalBody
			for k, v := range result.AuditFields {
				auditParams[k] = v
			}
		}
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
	// Sixth migrated call site through pipeline: task_approval_reply.
	{
		pipeReq := &pipelineReadOnlyRequest{
			provider:       provider,
			httpReq:        r,
			body:           body,
			userID:         agent.UserID,
			agentID:        agent.ID,
			conversationID: conversationID,
		}
		result, err := runSinglePolicy(r.Context(), pipeReq, policies.NewTaskApprovalReply(h.PendingApprovals, agent))
		if err != nil {
			auditStatus = http.StatusInternalServerError
			auditDecide = "deny"
			auditOutcome = "pipeline_error"
			auditReason = err.Error()
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusInternalServerError, "PIPELINE_ERROR", "internal pipeline error: "+err.Error()+". Please retry.")
			return
		}
		if result.DenyReason != "" {
			auditStatus = http.StatusBadRequest
			auditDecide = "deny"
			auditOutcome = "malformed_request"
			auditReason = result.DenyReason
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadRequest, "MALFORMED_REQUEST", "couldn't process the task-approval reply: "+result.DenyReason+". Please retry.")
			return
		}
		if string(result.FinalBody) != string(body) {
			body = result.FinalBody
			if _, err := parser.ParseRequest(body); err != nil {
				auditStatus = http.StatusBadRequest
				auditDecide = "deny"
				auditOutcome = "malformed_request"
				auditReason = err.Error()
				h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadRequest, "MALFORMED_REQUEST", "couldn't parse this request: "+err.Error()+". This usually means a client bug; please retry.")
				return
			}
		}
		for k, v := range result.AuditFields {
			auditParams[k] = v
		}
	}

	// Inline task approval: when the user's "approve"/"deny" reply
	// resolves an awaiting_task_approval hold, create the task and
	// rewrite the user message so the LLM gets clean context (rather
	// than a synthesized cat-heredoc tool_use that confuses the model
	// into re-POSTing /api/control/tasks).
	inlineApprovalConsumed := false
	// Seventh migrated call site through pipeline: inline_task_intercept.
	{
		pipeReq := &pipelineReadOnlyRequest{
			provider:       provider,
			httpReq:        r,
			body:           body,
			userID:         agent.UserID,
			agentID:        agent.ID,
			conversationID: conversationID,
		}
		result, err := runSinglePolicy(r.Context(), pipeReq, policies.NewInlineTaskIntercept(
			h.PendingApprovals, agent, h.InlineTaskCreator, h.AuditEmitter, requestID, h.InlineApprovalOutcomes, h.TaskCheckouts,
		))
		if err != nil {
			auditStatus = http.StatusInternalServerError
			auditDecide = "deny"
			auditOutcome = "pipeline_error"
			auditReason = err.Error()
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusInternalServerError, "PIPELINE_ERROR", "internal pipeline error: "+err.Error()+". Please retry.")
			return
		}
		if result.DenyReason != "" {
			auditStatus = http.StatusBadRequest
			auditDecide = "deny"
			auditOutcome = "malformed_request"
			auditReason = result.DenyReason
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadRequest, "MALFORMED_REQUEST", "couldn't process the inline-approval reply: "+result.DenyReason+". Please retry.")
			return
		}
		if string(result.FinalBody) != string(body) {
			body = result.FinalBody
			if _, err := parser.ParseRequest(body); err != nil {
				auditStatus = http.StatusBadRequest
				auditDecide = "deny"
				auditOutcome = "malformed_request"
				auditReason = err.Error()
				h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadRequest, "MALFORMED_REQUEST", "couldn't parse this request: "+err.Error()+". This usually means a client bug; please retry.")
				return
			}
			inlineApprovalConsumed = true
		}
		for k, v := range result.AuditFields {
			auditParams[k] = v
		}
	}

	// Persistent inline-approval context augmentation. The harness
	// records what the user typed ("approve") not our one-shot
	// rewrite ("approve [Clawvisor: ...]"), so on subsequent turns
	// the context is lost and the model duplicates work
	// (re-POSTs /api/control/tasks, re-emits tool_use). Walk conversation
	// history and re-inject the persistent context on every request.
	//
	// Fourth migrated call site through pipeline.
	{
		pipeReq := &pipelineReadOnlyRequest{
			provider:       provider,
			httpReq:        r,
			body:           body,
			userID:         agent.UserID,
			agentID:        agent.ID,
			conversationID: conversationID,
		}
		result, err := runSinglePolicy(r.Context(), pipeReq, policies.NewInlineTaskAugment(h.InlineApprovalOutcomes))
		if err != nil {
			h.Logger.WarnContext(r.Context(), "lite-proxy inline task augmentation pipeline failed",
				"request_id", requestID, "agent_id", agent.ID, "err", err.Error())
		} else {
			body = result.FinalBody
			for k, v := range result.AuditFields {
				auditParams[k] = v
			}
		}
	}
	reqSummary := liteProxyRequestDebugSummary(provider, body)
	h.ensureDefaultToolRules(r.Context(), agent, reqSummary.AvailableTools)
	// Fifth migrated call site through pipeline: control_notice. The
	// policy's gating mirrors shouldInjectLiteControlNotice (URL path
	// + tools[] declared); the AvailableToolsFn and ToolRulesLoader
	// callbacks delegate to existing handler helpers so the policy
	// stays Store-decoupled.
	{
		availableToolsFn := func(p conversation.Provider, b []byte) []string {
			return liteProxyRequestDebugSummary(p, b).AvailableTools
		}
		toolRulesLoader := func(ctx context.Context, _, _ string) []*store.RuntimePolicyRule {
			_, rules, _, loadErr := h.loadLiteProxyDecisionInputs(ctx, agent)
			if loadErr != nil {
				h.Logger.WarnContext(ctx, "lite-proxy notice injection skipped: decision-input load failed",
					"agent_id", agent.ID, "err", loadErr.Error())
				return nil
			}
			return rules
		}
		pipeReq := &pipelineReadOnlyRequest{
			provider: provider,
			httpReq:  r,
			body:     body,
			userID:   agent.UserID,
			agentID:  agent.ID,
		}
		result, err := runSinglePolicy(r.Context(), pipeReq, policies.NewControlNotice(h.ControlBaseURL, availableToolsFn, toolRulesLoader))
		if err != nil {
			auditStatus = http.StatusInternalServerError
			auditDecide = "deny"
			auditOutcome = "pipeline_error"
			auditReason = err.Error()
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusInternalServerError, "PIPELINE_ERROR", "internal pipeline error: "+err.Error()+". Please retry.")
			return
		}
		if result.DenyReason != "" {
			auditStatus = http.StatusBadRequest
			auditDecide = "deny"
			auditOutcome = "malformed_request"
			auditReason = result.DenyReason
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadRequest, "MALFORMED_REQUEST", "couldn't inject the control notice: "+result.DenyReason+". Please retry.")
			return
		}
		if string(result.FinalBody) != string(body) {
			body = result.FinalBody
			if _, err := parser.ParseRequest(body); err != nil {
				auditStatus = http.StatusBadRequest
				auditDecide = "deny"
				auditOutcome = "malformed_request"
				auditReason = err.Error()
				h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadRequest, "MALFORMED_REQUEST", "couldn't parse this request: "+err.Error()+". This usually means a client bug; please retry.")
				return
			}
			reqSummary = liteProxyRequestDebugSummary(provider, body)
		}
		for k, v := range result.AuditFields {
			auditParams[k] = v
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
		// Eighth migrated call site through pipeline:
		// ApprovalRelease wraps the tryApprovalRelease helper extracted
		// from the legacy maybeHandleLiteApprovalRelease method.
		// ShortCircuit fires when a release is handled; the handler
		// writes the synthesized response and returns.
		pipeReq := &pipelineReadOnlyRequest{
			provider:       provider,
			httpReq:        r,
			body:           body,
			userID:         agent.UserID,
			agentID:        agent.ID,
			conversationID: conversationID,
		}
		resolver := func(ctx context.Context) llmproxy.ReleaseResult {
			return h.tryApprovalRelease(r, agent, provider, requestID, conversationID, body)
		}
		preResult, err := runSinglePolicy(r.Context(), pipeReq, policies.NewApprovalRelease(resolver))
		if err != nil {
			auditStatus = http.StatusInternalServerError
			auditDecide = "deny"
			auditOutcome = "pipeline_error"
			auditReason = err.Error()
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusInternalServerError, "PIPELINE_ERROR", "internal pipeline error: "+err.Error()+". Please retry.")
			return
		}
		for k, v := range preResult.AuditFields {
			auditParams[k] = v
		}
		if sc := preResult.ShortCircuit; sc != nil {
			// Translate the synthesized response into the handler's
			// response-write protocol. Mirrors what the legacy
			// maybeHandleLiteApprovalRelease method did inline.
			auditStatus = sc.StatusCode
			if decision, _ := preResult.AuditFields["approval_release_decision"].(string); decision != "" {
				auditDecide = decision
			}
			if outcome, _ := preResult.AuditFields["approval_release_outcome"].(string); outcome != "" {
				auditOutcome = outcome
			}
			if reason, _ := preResult.AuditFields["approval_release_reason"].(string); reason != "" {
				auditReason = reason
			}
			// The decision-input-load-failed sentinel translates to the
			// DECISION_INPUT_UNAVAILABLE error response shape.
			if auditOutcome == "decision_input_load_failed" {
				h.Logger.WarnContext(r.Context(), "lite-proxy approval-release decision-input load failed; failing closed",
					"request_id", requestID, "agent_id", agent.ID, "err", auditReason)
				h.writeLiteProxyError(w, r, agent, provider, body, requestID, sc.StatusCode, "DECISION_INPUT_UNAVAILABLE",
					"couldn't load authorization data right now. Please retry in a moment.")
				return
			}
			h.Logger.DebugContext(r.Context(), "lite-proxy approval release handled (via pipeline)",
				"request_id", requestID,
				"agent_id", agent.ID,
				"provider", string(provider),
				"http_status", sc.StatusCode,
				"decision", auditDecide,
				"outcome", auditOutcome,
				"reason", auditReason,
			)
			if len(sc.Body) == 0 {
				h.writeLiteProxyError(w, r, agent, provider, body, requestID, sc.StatusCode, "APPROVAL_RELEASE_ERROR", auditReason)
				return
			}
			for k, v := range sc.Headers {
				w.Header().Set(k, v)
			}
			w.Header().Set("Cache-Control", "no-cache")
			if h.RawIOLogger != nil {
				bodyStr, bodyEnc := llmproxy.EncodeBody(sc.Body)
				h.RawIOLogger.Emit(llmproxy.RawIOEvent{
					Phase:        "harness_response",
					RequestID:    requestID,
					UserID:       agent.UserID,
					AgentID:      agent.ID,
					Provider:     string(provider),
					Method:       r.Method,
					Path:         r.URL.RequestURI(),
					Status:       sc.StatusCode,
					ContentType:  sc.Headers["Content-Type"],
					Headers:      llmproxy.SafeHeaderSnapshot(w.Header()),
					Body:         bodyStr,
					BodyEncoding: bodyEnc,
					BodyBytes:    len(sc.Body),
					Marker:       "synth_release_" + auditOutcome,
				})
			}
			w.WriteHeader(sc.StatusCode)
			_, _ = io.Copy(w, bytes.NewReader(sc.Body))
			return
		}
	}
	// Snapshot the pre-strip body for the first-turn routing-notice
	// detector below. StripSyntheticApprovalHistory removes assistant
	// approval-prompt turns and their bare "approve" replies so the
	// upstream model doesn't see synthesized scaffolding it could
	// hallucinate; from the user's POV those turns still happened.
	// Detecting on the post-strip body would misclassify
	// "first user prompt → tool_use → approval → continuation" as a
	// fresh turn-1 request and re-prepend the routing notice. The
	// pre-strip body reflects what the harness actually shipped, which
	// is the right input for the first-turn question.
	bodyForFirstTurnDetect := body
	// First-turn detection feeds two side-by-side decisions:
	//
	//  1. Conversation-ID mint. On harnesses without a native session
	//     identifier (today: OpenAI Chat Completions), turn-1 of a
	//     fresh conversation has no echoed marker yet, so
	//     ConversationID() returned the user-message fingerprint.
	//     Override it with a freshly minted "cv-conv-…" value and embed
	//     that value in the response-side routing notice so the
	//     harness round-trips it back to us in assistant history on
	//     turn 2+. The state we key by conversation ID (pending-approval
	//     holds, task-checkout focus) is created downstream under this
	//     minted value, so the next-turn marker-echo resolves cleanly
	//     to the same bucket.
	//
	//  2. Routing-notice prepend. Same firstTurn predicate selects
	//     which response gets the human-visible Clawvisor notice.
	firstTurn := !llmproxy.HasInboundAssistantTurn(provider, bodyForFirstTurnDetect)
	mintedConversationID := ""
	if firstTurn && provider == conversation.ProviderOpenAI && conversation.IsOpenAIChatCompletionsEndpoint(r) {
		minted, mintErr := conversation.NewConversationID()
		if mintErr != nil {
			// Mint failure is rare (crypto/rand outage) and recoverable:
			// fall through to the fingerprint that ConversationID()
			// already produced. The cost is just a degraded scope key
			// for this conversation, not a request failure.
			h.Logger.WarnContext(r.Context(), "lite-proxy conversation id mint failed; falling through to fingerprint",
				"request_id", requestID, "agent_id", agent.ID, "err", mintErr.Error())
		} else {
			mintedConversationID = minted
			conversationID = minted
			auditParams["conversation_id"] = minted
			auditParams["conversation_id_minted"] = true
		}
	}
	auditParams["first_turn"] = firstTurn
	auditParams["conversation_id_source"] = liteProxyConversationIDSource(provider, r, conversationID, mintedConversationID)
	// Third migrated call site through pipeline.
	{
		pipeReq := &pipelineReadOnlyRequest{
			provider:       provider,
			httpReq:        r,
			body:           body,
			userID:         agent.UserID,
			agentID:        agent.ID,
			conversationID: conversationID,
		}
		result, err := runSinglePolicy(r.Context(), pipeReq, policies.NewSyntheticHistoryStrip())
		if err != nil {
			h.Logger.WarnContext(r.Context(), "lite-proxy synthetic approval history strip pipeline failed",
				"request_id", requestID, "agent_id", agent.ID, "err", err.Error())
		} else {
			if string(result.FinalBody) != string(body) {
				body = result.FinalBody
				reqSummary = liteProxyRequestDebugSummary(provider, body)
			}
			for k, v := range result.AuditFields {
				auditParams[k] = v
			}
		}
	}

	upstreamURL := ""
	if h.Forwarder != nil {
		upstreamPath := llmproxy.ProviderPath(provider, r.URL.Path)
		if u, urlErr := h.Forwarder.Upstream.URL(provider, upstreamPath); urlErr == nil {
			u.RawQuery = r.URL.RawQuery
			upstreamURL = u.String()
		} else {
			h.Logger.DebugContext(r.Context(), "lite-proxy upstream URL build failed",
				"request_id", requestID,
				"agent_id", agent.ID,
				"provider", string(provider),
				"path", upstreamPath,
				"query", r.URL.RawQuery,
				"err", urlErr.Error(),
			)
		}
	}
	h.Logger.DebugContext(r.Context(), "lite-proxy forwarding upstream",
		"request_id", requestID,
		"agent_id", agent.ID,
		"provider", string(provider),
		"inbound_path", r.URL.RequestURI(),
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
			code, outcome, message := upstreamCredMissingError(r, agent, provider, h.DashboardBaseURL)
			auditOutcome = outcome
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusUnauthorized, code, message)
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
		h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadGateway, "UPSTREAM_ERROR", "couldn't reach the upstream provider. Please retry; if this persists, check the Clawvisor daemon logs.")
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
		if conversation.IsSSEContentType(upstreamCT) {
			candidateTasks, toolRules, egressRules, decisionLoadErr := h.loadLiteProxyDecisionInputs(r.Context(), agent)
			if decisionLoadErr != nil {
				h.Logger.WarnContext(r.Context(), "lite-proxy decision-input load failed; failing closed",
					"request_id", requestID, "agent_id", agent.ID, "err", decisionLoadErr.Error())
				auditStatus = http.StatusServiceUnavailable
				auditDecide = "deny"
				auditOutcome = "decision_input_load_failed"
				auditReason = decisionLoadErr.Error()
				h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusServiceUnavailable, "DECISION_INPUT_UNAVAILABLE",
					"couldn't load authorization data right now. Please retry in a moment.")
				return
			}
			preferredTaskID, preferredTaskErr := h.checkedOutTaskID(r.Context(), agent, conversationID, candidateTasks)
			if preferredTaskErr != nil {
				h.Logger.WarnContext(r.Context(), "lite-proxy task checkout lookup failed; continuing without preferred task",
					"request_id", requestID, "agent_id", agent.ID, "err", preferredTaskErr.Error())
				auditParams["task_checkout_unavailable"] = true
				preferredTaskID = ""
			}
			if preferredTaskID != "" {
				auditTaskID = preferredTaskID
			}
			h.Logger.DebugContext(r.Context(), "lite-proxy decision inputs loaded (streaming)",
				"request_id", requestID,
				"agent_id", agent.ID,
				"provider", string(provider),
				"posture", string(liteProxyDecisionPosture(agent)),
				"candidate_tasks", len(candidateTasks),
				"tool_rules", len(toolRules),
				"egress_rules", len(egressRules),
				"preferred_task_id", preferredTaskID,
			)

			recentTurns := llmproxy.ExtractRecentHumanTurns(llmproxy.ExtractHumanTurnsRequest{
				Provider: provider,
				Body:     body,
			})
			autoApproveThreshold := agentConversationAutoApproveThreshold(agent)

			callerToken := middleware.CallerTokenFromContext(r.Context())
			if callerToken == "" {
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

			cfg := llmproxy.PostprocessConfig{
				Inspector:                        h.Inspector,
				RewriteOpts:                      opts,
				Store:                            h.Store,
				AgentUserID:                      agent.UserID,
				AgentID:                          agent.ID,
				ConversationID:                   conversationID,
				Audit:                            h.AuditEmitter,
				RequestID:                        requestID,
				Catalog:                          catalogIface,
				TaskScope:                        h.TaskScope,
				IntentVerifier:                   h.IntentVerifier,
				Posture:                          liteProxyDecisionPosture(agent),
				CandidateTasks:                   candidateTasks,
				ToolRules:                        toolRules,
				EgressRules:                      egressRules,
				PreferredTaskID:                  preferredTaskID,
				PendingApprovals:                 h.PendingApprovals,
				ControlBaseURL:                   h.ControlBaseURL,
				CallerNonces:                     h.CallerNonces,
				Trace:                            h.TraceLogger,
				TaskRiskAssessor:                 h.taskRiskBridge(),
				AgentName:                        agent.Name,
				RecentUserTurns:                  recentTurns,
				ConversationAutoApproveThreshold: autoApproveThreshold,
				InlineTaskCreator:                h.InlineTaskCreator,
				Checkouts:                        h.TaskCheckouts,
				DefaultTaskExpirySeconds:         h.DefaultTaskExpirySeconds,
			}
			// First-turn routing notice. Mirrors the buffered path's
			// invocation below (search "first-turn routing notice").
			// The streaming SSE injector inside PostprocessStream wraps
			// the writer so the notice surfaces as the leading content
			// block / output_item and downstream indices are shifted by
			// +1. Skip on non-200 — error bodies aren't shaped to take
			// the prepend and would be soft no-ops anyway.
			if resp.StatusCode == http.StatusOK && firstTurn {
				cfg.FirstTurnNotice = llmproxy.RenderAgentRoutingNotice(agent.Name, mintedConversationID)
			}

			w.Header().Del("Content-Length")
			w.Header().Del("Content-Encoding")
			if upstreamCT != "" {
				w.Header().Set("Content-Type", upstreamCT)
			}
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")

			streamResp := newDelayedHeaderWriter(w, resp.StatusCode)
			flushingW := newFlushingWriter(streamResp)
			timingW := newStreamingTimingWriter(flushingW, h.Logger, requestID, string(provider), upstreamHeadersMs)
			emittedStream := newLimitedCaptureWriter(h.MaxResponseBytes)
			streamW := io.MultiWriter(timingW, emittedStream)

			var capturedUpstream bytes.Buffer
			teeReader := io.TeeReader(resp.Body, &capturedUpstream)

			processed, err := llmproxy.PostprocessStream(r.Context(), r, teeReader, streamW, upstreamCT, cfg)
			if err != nil {
				h.Logger.WarnContext(r.Context(), "lite-proxy postprocess streaming error",
					"request_id", requestID, "agent_id", agent.ID, "err", err.Error())
				auditStatus = http.StatusBadGateway
				auditOutcome = "postprocess_stream_error"
				auditReason = err.Error()
				if !streamResp.WroteHeader() {
					clearHeaders(w.Header())
					h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadGateway, "POSTPROCESS_STREAM_ERROR",
						"couldn't process the upstream stream. Please retry; details are in the Clawvisor audit log.")
				} else {
					llmproxy.WriteStreamError(streamW, r, provider, processed.StreamingResult,
						"[Clawvisor] The upstream connection was lost before the response completed. The content above may be incomplete. Please retry.")
				}
				return
			}

			h.Logger.DebugContext(r.Context(), "lite-proxy streaming postprocess complete",
				"request_id", requestID,
				"agent_id", agent.ID,
				"provider", string(provider),
				"status", resp.StatusCode,
				"rewritten", processed.Rewritten,
				"decisions", len(processed.Decisions),
				"skipped_reason", processed.SkippedReason,
			)

			if auditTaskID == "" {
				for _, dec := range processed.Decisions {
					if dec.Verdict.ContinueWithToolResult != "" && dec.Verdict.CreatedTaskID != "" {
						auditTaskID = dec.Verdict.CreatedTaskID
						break
					}
				}
			}

			firstUpstreamCT := upstreamCT
			streamStatus := resp.StatusCode
			if len(processed.ContinuationToolResults) > 0 {
				contFinal, contStatus, contCT, contUsage, contErr := h.tryContinuation(r, agent, provider, requestID, body, capturedUpstream.Bytes(), upstreamCT, resp.StatusCode, processed, cfg)
				switch {
				case contErr != nil:
					h.Logger.WarnContext(r.Context(), "lite-proxy streaming continuation failed",
						"request_id", requestID, "agent_id", agent.ID, "err", contErr.Error())
					if !streamResp.WroteHeader() {
						clearHeaders(w.Header())
						auditStatus = http.StatusBadGateway
						auditOutcome = "streaming_continuation_failed"
						auditReason = contErr.Error()
						h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadGateway, "STREAMING_CONTINUATION_FAILED",
							"couldn't continue the streamed tool result. Please retry; details are in the Clawvisor audit log.")
					}
					return
				case contFinal != nil:
					if contFinal.SkippedReason != "" {
						h.Logger.WarnContext(r.Context(), "lite-proxy streaming continuation postprocess reported SkippedReason",
							"request_id", requestID,
							"agent_id", agent.ID,
							"skipped_reason", contFinal.SkippedReason,
							"body_bytes", len(contFinal.Body),
						)
						if !streamResp.WroteHeader() {
							clearHeaders(w.Header())
							auditStatus = http.StatusBadGateway
							auditOutcome = "streaming_continuation_postprocess_error"
							auditReason = contFinal.SkippedReason
							h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadGateway, "STREAMING_CONTINUATION_POSTPROCESS_ERROR",
								"couldn't process the streamed continuation response. Please retry; details are in the Clawvisor audit log.")
						}
						return
					}
					originalStreamResult := processed.StreamingResult
					processed = *contFinal
					if contStatus != 0 {
						streamStatus = contStatus
						auditStatus = contStatus
						auditOutcome = outcomeFromStatus(contStatus)
					}
					if contCT != "" && contCT != upstreamCT && !streamResp.WroteHeader() {
						upstreamCT = contCT
						w.Header().Set("Content-Type", contCT)
					}
					if contUsage != nil {
						mergeAuditUsage(&auditUsage, auditParams, contUsage, r.Context(), h.Logger, requestID, agent.ID, "streaming_continuation")
					}
					if len(contFinal.Body) > 0 {
						continuationBody, spliceErr := spliceStreamingContinuationBody(provider, originalStreamResult, contCT, contFinal.Body)
						if spliceErr != nil {
							h.Logger.WarnContext(r.Context(), "lite-proxy streaming continuation splice failed",
								"request_id", requestID, "agent_id", agent.ID, "err", spliceErr.Error())
							if !streamResp.WroteHeader() {
								clearHeaders(w.Header())
								auditStatus = http.StatusBadGateway
								auditOutcome = "streaming_continuation_splice_failed"
								auditReason = spliceErr.Error()
								h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadGateway, "STREAMING_CONTINUATION_SPLICE_FAILED",
									"couldn't merge the streamed continuation response. Please retry; details are in the Clawvisor audit log.")
							}
							return
						}
						if _, writeErr := streamW.Write(continuationBody); writeErr != nil {
							h.Logger.WarnContext(r.Context(), "lite-proxy streaming continuation write failed",
								"request_id", requestID, "agent_id", agent.ID, "err", writeErr.Error())
							return
						}
					}
				}
			}

			streamResp.Commit()

			if resp.StatusCode >= 400 {
				h.Logger.WarnContext(r.Context(), "lite-proxy streaming upstream error body",
					"request_id", requestID,
					"agent_id", agent.ID,
					"provider", string(provider),
					"status", resp.StatusCode,
					"body_preview", truncateForLog(capturedUpstream.String(), 2048),
				)
			}

			if resp.StatusCode < 400 {
				if usage := llmproxy.ExtractUsage(provider, firstUpstreamCT, capturedUpstream.Bytes(), reqSummary.Model); usage.Found {
					u := usage
					mergeAuditUsage(&auditUsage, auditParams, &u, r.Context(), h.Logger, requestID, agent.ID, "streaming")
				}
			}
			if auditUsage != nil {
				auditParams["usage_input_tokens"] = auditUsage.Usage.InputTokens
				auditParams["usage_output_tokens"] = auditUsage.Usage.OutputTokens
				if auditUsage.Usage.CacheReadTokens > 0 {
					auditParams["usage_cache_read_tokens"] = auditUsage.Usage.CacheReadTokens
				}
				if auditUsage.Usage.CacheWriteTokens > 0 {
					auditParams["usage_cache_write_tokens"] = auditUsage.Usage.CacheWriteTokens
				}
				if auditUsage.Usage.CacheWrite1hTokens > 0 {
					auditParams["usage_cache_write_1h_tokens"] = auditUsage.Usage.CacheWrite1hTokens
				}
			}

			if h.RawIOLogger != nil {
				emittedBody := emittedStream.Bytes()
				bodyStr, bodyEnc := llmproxy.EncodeBody(emittedBody)
				marker := "streaming"
				if processed.Rewritten {
					marker = "streaming_rewritten"
				}
				h.RawIOLogger.Emit(llmproxy.RawIOEvent{
					Phase:        "harness_response",
					RequestID:    requestID,
					UserID:       agent.UserID,
					AgentID:      agent.ID,
					Provider:     string(provider),
					Method:       r.Method,
					Path:         r.URL.RequestURI(),
					Status:       streamStatus,
					ContentType:  upstreamCT,
					Headers:      llmproxy.SafeHeaderSnapshot(w.Header()),
					Body:         bodyStr,
					BodyEncoding: bodyEnc,
					BodyBytes:    len(emittedBody),
					Marker:       marker,
				})
			}

			auditStatus = streamStatus
			if auditOutcome == "" {
				auditOutcome = outcomeFromStatus(streamStatus)
			}
			return
		}

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
		// Extract token usage on success (skip 4xx/5xx — they have no
		// token-bearing body). The deferred audit emit at the top of
		// serve() reads auditUsage / auditTaskID and writes one
		// llm_request_cost row when usage is found. Cost is best-
		// effort observability and never blocks the response. The
		// task-id link is filled in below, once preferredTaskID has
		// been resolved.
		if readErr == nil && resp.StatusCode < 400 {
			if usage := llmproxy.ExtractUsage(provider, upstreamCT, full, reqSummary.Model); usage.Found {
				u := usage
				auditUsage = &u
				auditParams["usage_input_tokens"] = usage.Usage.InputTokens
				auditParams["usage_output_tokens"] = usage.Usage.OutputTokens
				if usage.Usage.CacheReadTokens > 0 {
					auditParams["usage_cache_read_tokens"] = usage.Usage.CacheReadTokens
				}
				if usage.Usage.CacheWriteTokens > 0 {
					auditParams["usage_cache_write_tokens"] = usage.Usage.CacheWriteTokens
				}
			}
		}
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
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadGateway, "UPSTREAM_READ_ERROR", "lost the connection to the upstream provider while reading its response. Please retry.")
			return
		}
		if resp.StatusCode >= 400 {
			h.Logger.WarnContext(r.Context(), "lite-proxy upstream error body",
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
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusServiceUnavailable, "DECISION_INPUT_UNAVAILABLE",
				"couldn't load authorization data right now. Please retry in a moment.")
			return
		}
		preferredTaskID, preferredTaskErr := h.checkedOutTaskID(r.Context(), agent, conversationID, candidateTasks)
		if preferredTaskErr != nil {
			h.Logger.WarnContext(r.Context(), "lite-proxy task checkout lookup failed; continuing without preferred task",
				"request_id", requestID, "agent_id", agent.ID, "err", preferredTaskErr.Error())
			auditParams["task_checkout_unavailable"] = true
			preferredTaskID = ""
		}
		if preferredTaskID != "" {
			auditTaskID = preferredTaskID
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
			DefaultTaskExpirySeconds:         h.DefaultTaskExpirySeconds,
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

		// Conversation continuation. When one or more tool_use verdicts
		// asked the proxy to feed a synthetic tool_result back to the
		// upstream (instead of bouncing to the user as an assistant text
		// turn), build a continuation request, forward upstream, and run
		// postprocess again on the new response. The harness then sees
		// the model's next tool_use rather than a "task was approved"
		// terminal message — letting auto-approved tasks proceed
		// seamlessly. Recursion is bounded by construction: tryContinuation
		// performs exactly one inline Postprocess pass and does not loop,
		// so even if the second pass fires the auto-approve gate again it
		// falls through to SubstituteWith (no further forward, no further
		// Postprocess). On any failure path the handler falls back to the
		// original `processed` (which still carries SubstituteWith as a
		// terminal assistant text), so the harness never sees an empty body.
		{
			contFinal, contStatus, contCT, contUsage, contErr := h.tryContinuation(r, agent, provider, requestID, body, full, upstreamCT, resp.StatusCode, processed, llmproxy.PostprocessConfig{
				Inspector:                        h.Inspector,
				RewriteOpts:                      opts,
				Store:                            h.Store,
				AgentUserID:                      agent.UserID,
				AgentID:                          agent.ID,
				ConversationID:                   conversationID,
				Audit:                            h.AuditEmitter,
				RequestID:                        requestID,
				Catalog:                          catalogIface,
				TaskScope:                        h.TaskScope,
				IntentVerifier:                   h.IntentVerifier,
				Posture:                          liteProxyDecisionPosture(agent),
				CandidateTasks:                   candidateTasks,
				ToolRules:                        toolRules,
				EgressRules:                      egressRules,
				PreferredTaskID:                  preferredTaskID,
				PendingApprovals:                 h.PendingApprovals,
				ControlBaseURL:                   h.ControlBaseURL,
				CallerNonces:                     h.CallerNonces,
				Trace:                            h.TraceLogger,
				TaskRiskAssessor:                 h.taskRiskBridge(),
				AgentName:                        agent.Name,
				RecentUserTurns:                  recentTurns,
				ConversationAutoApproveThreshold: autoApproveThreshold,
				InlineTaskCreator:                h.InlineTaskCreator,
				Checkouts:                        h.TaskCheckouts,
				DefaultTaskExpirySeconds:         h.DefaultTaskExpirySeconds,
			})
			switch {
			case contErr != nil:
				h.Logger.WarnContext(r.Context(), "lite-proxy continuation failed; falling back to substitute response",
					"request_id", requestID, "agent_id", agent.ID, "err", contErr.Error())
			case contFinal != nil:
				// Treat any SkippedReason on the continuation's
				// postprocess as a continuation failure regardless of
				// whether a (possibly partial) body came back.
				// SkippedReason indicates the rewriter couldn't finish
				// its pass cleanly; swapping that body in would mask
				// the original processed.SubstituteWith fallback and
				// could leak partially-rewritten content (e.g. a
				// literal autovault_… placeholder that never got
				// resolved). Fall back to the original `processed`,
				// matching the pre-continuation fail-closed posture.
				if contFinal.SkippedReason != "" {
					h.Logger.WarnContext(r.Context(), "lite-proxy continuation postprocess reported SkippedReason; falling back to substitute response",
						"request_id", requestID,
						"agent_id", agent.ID,
						"skipped_reason", contFinal.SkippedReason,
						"body_bytes", len(contFinal.Body),
					)
					break
				}
				processed = *contFinal
				if contStatus != 0 {
					resp.StatusCode = contStatus
					auditStatus = contStatus
					auditOutcome = outcomeFromStatus(contStatus)
				}
				if contCT != "" && contCT != upstreamCT {
					w.Header().Set("Content-Type", contCT)
				}
				// Reattribute the cost row ONLY when no prior task
				// attribution exists. The auto-approve gate in the
				// first postprocess pass minted a new task
				// (Verdict.CreatedTaskID) and the continuation does
				// THAT task's work, so when the conversation had no
				// checkout yet (first turn) the cost belongs there
				// rather than on NULL. But when the user already had
				// a task checked out for this conversation,
				// preferredTaskID populated auditTaskID earlier — a
				// real, specific attribution we should preserve
				// rather than overwrite with the just-minted task.
				if auditTaskID == "" {
					for _, dec := range processed.Decisions {
						if dec.Verdict.ContinueWithToolResult != "" && dec.Verdict.CreatedTaskID != "" {
							auditTaskID = dec.Verdict.CreatedTaskID
							break
						}
					}
				}
				// Roll the continuation's tokens into the per-request
				// cost. We expect both calls to report the same
				// model (BuildContinuationBody preserves the inbound
				// `model` field), so summing token buckets is safe
				// and pricing.Compute will price the combined sum
				// against one model row. Defend against a future
				// regression where the upstream resolves to a
				// different model on the continuation: drop the
				// continuation's usage rather than silently bill it
				// at the first call's rate.
				if contUsage != nil {
					switch {
					case auditUsage == nil:
						auditUsage = contUsage
						auditParams["continuation_usage_recorded"] = true
					case pricingModelMatch(auditUsage.Model, contUsage.Model):
						auditUsage.Usage.InputTokens += contUsage.Usage.InputTokens
						auditUsage.Usage.OutputTokens += contUsage.Usage.OutputTokens
						auditUsage.Usage.CacheReadTokens += contUsage.Usage.CacheReadTokens
						auditUsage.Usage.CacheWriteTokens += contUsage.Usage.CacheWriteTokens
						auditUsage.Usage.CacheWrite1hTokens += contUsage.Usage.CacheWrite1hTokens
						auditParams["continuation_usage_recorded"] = true
					default:
						h.Logger.WarnContext(r.Context(), "lite-proxy continuation usage dropped: model mismatch with original call",
							"request_id", requestID,
							"agent_id", agent.ID,
							"original_model", auditUsage.Model,
							"continuation_model", contUsage.Model,
						)
						auditParams["continuation_usage_dropped_model_mismatch"] = true
					}
				}
			}
		}

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
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadGateway, "POSTPROCESS_ERROR",
				"couldn't process the upstream response. Please retry; details are in the Clawvisor audit log.")
			return
		}
		// First-turn routing notice. When the inbound transcript carries
		// no prior assistant turn, this is the user's opening prompt of
		// a fresh conversation — prepend a one-liner so they see that
		// Clawvisor is intermediating and which agent identity is in
		// use. Only fires on 200; non-success bodies aren't shaped to
		// accept the prepend and would be soft no-ops anyway. The
		// per-tool-use auto-approve notice is handled separately on the
		// continuation path and is additive with this one.
		if resp.StatusCode == http.StatusOK && firstTurn {
			// mintedConversationID is non-empty only on Chat Completions
			// turn 1 (where no native session ID exists). For Anthropic
			// and OpenAI Responses the renderer drops the marker
			// argument and emits the existing notice unchanged.
			notice := llmproxy.RenderAgentRoutingNotice(agent.Name, mintedConversationID)
			pre, changed, prependErr := llmproxy.PrependAssistantNotice(provider, processed.ContentType, processed.Body, notice)
			switch {
			case prependErr != nil:
				h.Logger.WarnContext(r.Context(), "lite-proxy first-turn notice prepend failed; returning unannotated body",
					"request_id", requestID, "agent_id", agent.ID, "err", prependErr.Error())
			case changed:
				processed.Body = pre
				// Mark Rewritten so the Content-Length / Content-Encoding
				// cleanup below runs — the prepended body's length no
				// longer matches the upstream header.
				processed.Rewritten = true
			}
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

// upstreamCredMissingError shapes the user-facing error for the "no
// upstream credential available" case. The user message is the same
// either way — just "get a key, paste it here" — but the audit
// outcome distinguishes passthrough-with-no-bearer from a plain vault
// miss so operators can still tell the failure modes apart.
//
// dashboardBaseURL is the deployment's externally reachable dashboard
// host. Falls back to the build-env default in pkg/version when
// empty — correct for hosted builds, wrong for local self-hosted, so
// the wiring in server.go should set this explicitly when it knows
// the right host (which is essentially always in practice).
func upstreamCredMissingError(r *http.Request, agent *store.Agent, provider conversation.Provider, dashboardBaseURL string) (code, outcome, message string) {
	if llmproxy.PassthroughUpstreamAuth(r.Context()) && !llmproxy.HasPassthroughBearer(r) {
		code, outcome = "UPSTREAM_AUTH_MISSING", "upstream_auth_missing_for_passthrough"
	} else {
		code, outcome = "UPSTREAM_KEY_MISSING", "upstream_key_missing"
	}
	providerName := "Anthropic"
	consoleURL := "https://console.anthropic.com/settings/keys"
	if provider == conversation.ProviderOpenAI {
		providerName = "OpenAI"
		consoleURL = "https://platform.openai.com/api-keys"
	}
	dashboardBase := strings.TrimRight(strings.TrimSpace(dashboardBaseURL), "/")
	if dashboardBase == "" {
		dashboardBase = version.DashboardURL()
	}
	message = "no " + providerName + " API key configured. Get one at " + consoleURL + " and paste it at " + dashboardBase + "/dashboard/agents/" + agent.ID + "."
	return code, outcome, message
}

// writeJSONError produces a uniform JSON error response. Use this only
// for pre-provider failures (no agent token, unknown route) where the
// harness can't be addressed in its native wire shape.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
		"code":  code,
	})
}

// writeLiteProxyError emits an error in the wire shape the harness
// expects (Anthropic SSE / OpenAI Responses SSE / Chat Completions
// SSE or JSON) so the user sees a recoverable inline message — "the
// approval expired, please retry" — instead of the harness's generic
// "model may not exist" fallback. A non-harness-shaped JSON body
// triggers that fallback on every CLI we ship to today.
//
// The wire response is HTTP 200 with a synthetic assistant text turn.
// The original error status/code/reason stays in audit logs and slog
// for operators. Callers still set audit* fields before invoking this
// helper; behavior beyond response synthesis matches writeJSONError.
//
// When provider is unsupported (or message is empty), falls back to
// writeJSONError with the supplied status/code/message.
func (h *LLMEndpointHandler) writeLiteProxyError(w http.ResponseWriter, r *http.Request, agent *store.Agent, provider conversation.Provider, requestBody []byte, requestID string, status int, code, message string) {
	clearMirroredUpstreamHeaders(w.Header())
	// Prefix every synthesized error so the user can tell it's from
	// Clawvisor and not from the model or the upstream provider. The
	// JSON fallback path keeps its `code` field for that role.
	branded := message
	if branded != "" && !strings.HasPrefix(branded, "Clawvisor") {
		branded = "Clawvisor: " + branded
	}
	synth, ok := conversation.SyntheticErrorResponse(r, provider, requestBody, branded)
	if !ok {
		writeJSONError(w, status, code, message)
		return
	}
	w.Header().Set("Content-Type", synth.ContentType)
	w.Header().Set("Cache-Control", "no-cache")
	if h.RawIOLogger != nil && agent != nil {
		bodyStr, bodyEnc := llmproxy.EncodeBody(synth.Body)
		h.RawIOLogger.Emit(llmproxy.RawIOEvent{
			Phase:        "harness_response",
			RequestID:    requestID,
			UserID:       agent.UserID,
			AgentID:      agent.ID,
			Provider:     string(provider),
			Method:       r.Method,
			Path:         r.URL.RequestURI(),
			Status:       http.StatusOK,
			ContentType:  synth.ContentType,
			Headers:      llmproxy.SafeHeaderSnapshot(w.Header()),
			Body:         bodyStr,
			BodyEncoding: bodyEnc,
			BodyBytes:    len(synth.Body),
			Marker:       "synth_error_" + code,
		})
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(synth.Body)
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

// tryContinuation inspects the just-completed postprocess result for
// tool_use verdicts that requested the proxy "continue the
// conversation" (i.e. auto-approved tasks). When any are present and
// the provider supports continuation, the handler builds a new
// request body that appends the upstream's assistant turn plus a
// synthetic user turn of tool_result blocks, forwards it upstream,
// and re-runs postprocess on the new response. The new processed
// result (along with its upstream status + content-type) is returned;
// the caller swaps it in for the original.
//
// Returns (nil, 0, "", nil) when no continuation was requested — the
// caller falls through to the existing path unchanged.
// Returns an error when continuation was requested but couldn't be
// completed (unsupported provider, malformed bodies, upstream failure);
// the caller logs and falls back to the original processed result,
// whose SubstituteWith fallback still surfaces the augmentation text
// to the harness as a terminal assistant turn.
func (h *LLMEndpointHandler) tryContinuation(
	r *http.Request,
	agent *store.Agent,
	provider conversation.Provider,
	requestID string,
	inboundBody []byte,
	upstreamBody []byte,
	upstreamCT string,
	upstreamStatus int,
	processed llmproxy.PostprocessResult,
	cfg llmproxy.PostprocessConfig,
) (*llmproxy.PostprocessResult, int, string, *llmproxy.ExtractUsageResult, error) {
	if upstreamStatus >= 400 {
		// Don't try to continue on top of an upstream error response —
		// the model never actually emitted a clean tool_use turn, and
		// the body shape may not match what extractAnthropicAssistantContent
		// expects.
		return nil, 0, "", nil, nil
	}
	var toolResults []llmproxy.ContinuationToolResult
	for _, dec := range processed.Decisions {
		if dec.Verdict.ContinueWithToolResult == "" {
			continue
		}
		toolResults = append(toolResults, llmproxy.ContinuationToolResult{
			ToolUseID: dec.ToolUse.ID,
			Content:   dec.Verdict.ContinueWithToolResult,
		})
	}
	if len(toolResults) == 0 {
		return nil, 0, "", nil, nil
	}
	// Tool_use / tool_result must be 1:1 for the upstream — Anthropic
	// and OpenAI Chat both 400 on an unbalanced continuation body. If
	// the assistant turn carried sibling tool_uses that were NOT
	// auto-approved (e.g. a Bash command we passed through alongside
	// the POST /api/control/tasks the gate intercepted), we'd
	// otherwise emit N tool_uses + len(toolResults) tool_results and
	// the upstream would reject the turn. Worse, the proxy's response
	// to the harness is the continuation: if we tried to continue
	// anyway and the model ran something, the harness would also
	// execute the passed-through Bash, double-running it. The safe
	// answer is to skip continuation entirely and fall back to the
	// substitute path — the user gets the [Clawvisor] bracketed
	// fallback turn, the model sees no continuation, and the
	// passed-through tool_use returns to its normal harness fate on
	// the model's next turn.
	if len(toolResults) != len(processed.Decisions) {
		// The auto-approved task has already been created by the
		// gate. The sibling tool_uses get dropped from the
		// substitute-rendered assistant turn (the rewriter's "any
		// blocked" branch substitutes the whole turn), so the
		// harness never sees them — that's the surprising part for
		// operators chasing "I approved the task but Bash never ran
		// after that." Record a dedicated audit row enumerating the
		// dropped tool names so the trail is greppable.
		var droppedNames []string
		var autoApprovedTUID, autoApprovedTaskID string
		for _, dec := range processed.Decisions {
			if dec.Verdict.ContinueWithToolResult != "" {
				autoApprovedTUID = dec.ToolUse.ID
				if autoApprovedTaskID == "" {
					autoApprovedTaskID = dec.Verdict.CreatedTaskID
				}
				continue
			}
			droppedNames = append(droppedNames, dec.ToolUse.Name)
		}
		h.Logger.WarnContext(r.Context(), "lite-proxy continuation skipped: sibling tool_uses in same turn would unbalance tool_use/tool_result count",
			"request_id", requestID,
			"agent_id", agent.ID,
			"task_id", autoApprovedTaskID,
			"tool_uses_in_turn", len(processed.Decisions),
			"continue_results", len(toolResults),
			"dropped_tools", droppedNames,
		)
		if h.AuditEmitter != nil {
			h.AuditEmitter.LogContinuationSkippedSiblingTools(r.Context(), agent, requestID, autoApprovedTaskID, autoApprovedTUID, droppedNames)
		}
		return nil, 0, "", nil, nil
	}
	contBody, err := llmproxy.BuildContinuationBody(provider, upstreamCT, inboundBody, upstreamBody, toolResults)
	if err != nil {
		return nil, 0, "", nil, fmt.Errorf("build continuation body: %w", err)
	}
	h.Logger.DebugContext(r.Context(), "lite-proxy continuation forwarding",
		"request_id", requestID,
		"agent_id", agent.ID,
		"provider", string(provider),
		"tool_results", len(toolResults),
		"body_bytes", len(contBody),
	)
	resp, err := h.Forwarder.Forward(r.Context(), agent.UserID, agent.ID, provider, r, contBody)
	if err != nil {
		return nil, 0, "", nil, fmt.Errorf("forward continuation: %w", err)
	}
	defer resp.Body.Close()
	full, readErr := readResponseLimited(resp.Body, h.MaxResponseBytes)
	if readErr != nil {
		return nil, 0, "", nil, fmt.Errorf("read continuation upstream: %w", readErr)
	}
	if resp.StatusCode >= 400 {
		return nil, 0, "", nil, fmt.Errorf("continuation upstream returned %d", resp.StatusCode)
	}
	contCT := resp.Header.Get("Content-Type")
	if contCT == "" {
		contCT = upstreamCT
	}
	// Extract usage on the continuation body so its tokens land in
	// the per-task cost rollup. Without this, any auto-approved
	// task that proceeds via continuation would under-report by
	// exactly the tokens the post-approval call burned — typically
	// the call that actually does the work. The continuation reuses
	// the inbound request's model (BuildContinuationBody preserves
	// it) so the upstream body will name the same model — no
	// requestModel fallback needed.
	var contUsage *llmproxy.ExtractUsageResult
	if u := llmproxy.ExtractUsage(provider, contCT, full, ""); u.Found {
		uc := u
		contUsage = &uc
	}
	// Refresh decision inputs before the continuation postprocess. The
	// original cfg.CandidateTasks was loaded at the top of serve(),
	// BEFORE the auto-approve gate created the new task — so it doesn't
	// include the task we just minted. Without this reload the model's
	// next tool_uses (Write, Bash, …) fall through to "no matching task
	// scope" and the harness shows the human-approval prompt again,
	// defeating the whole point of conversation auto-approval. ToolRules
	// and EgressRules rarely change inside a single inbound request, but
	// reloading them keeps the cfg internally consistent and absorbs any
	// concurrent rule updates for free. PreferredTaskID gets recomputed
	// from the checkouts cache (which the auto-approve path Set'd to the
	// new task) so the decision layer's task preference matches the
	// active checkout.
	refreshedCandidates, refreshedToolRules, refreshedEgressRules, refreshErr := h.loadLiteProxyDecisionInputs(r.Context(), agent)
	if refreshErr == nil {
		cfg.CandidateTasks = refreshedCandidates
		cfg.ToolRules = refreshedToolRules
		cfg.EgressRules = refreshedEgressRules
		if newPref, prefErr := h.checkedOutTaskID(r.Context(), agent, cfg.ConversationID, refreshedCandidates); prefErr == nil {
			cfg.PreferredTaskID = newPref
		}
	} else {
		// Don't fail the continuation on a decision-input refresh
		// hiccup — fall through with the stale cfg. The worst case is
		// the human-approval prompt we were trying to avoid, which is
		// the same behavior as a transient store outage on the
		// original request path.
		h.Logger.WarnContext(r.Context(), "lite-proxy continuation decision-input refresh failed; using pre-continuation snapshot",
			"request_id", requestID, "agent_id", agent.ID, "err", refreshErr.Error())
	}
	// Recursion is bounded by construction here: tryContinuation does
	// not loop, so the second Postprocess pass below runs at most once
	// per inbound harness request. If that second pass fires the
	// auto-approve gate again, the gate's verdict still carries
	// ContinueWithToolResult, but we never act on it — the caller
	// (serve()) doesn't re-invoke tryContinuation. The verdict's
	// SubstituteWith fallback renders as a terminal text turn instead,
	// which is the right behavior for a model that re-emits a task-
	// creation tool_use on the continuation.
	newProcessed := llmproxy.Postprocess(r, full, contCT, cfg)
	// Force Rewritten=true on a successful continuation swap. The
	// body now comes from a SECOND upstream call whose length almost
	// certainly differs from the first call's Content-Length (which
	// serve() mirrored into w.Header at the top of the handler) and
	// which may carry a different Content-Encoding. Without this flag,
	// the second Postprocess can legitimately report Rewritten=false
	// when the body itself was passthrough (plain text turn, no
	// tool_use to rewrite) — and serve()'s `if processed.Rewritten`
	// header-clear block would skip dropping Content-Length /
	// Content-Encoding. Go would then truncate the harness write to
	// the stale length or the harness would try to gunzip our
	// plaintext. Co-locating the flag here (rather than in serve())
	// keeps the invariant tight: any non-nil return from
	// tryContinuation has had its origin headers invalidated by the
	// upstream swap, and the caller can rely on Rewritten=true to
	// route through the normal post-rewrite cleanup.
	newProcessed.Rewritten = true

	// User-facing notices. The auto-approve gate records a one-line
	// notice on each verdict via PrependAssistantNotice; we collect
	// them here and inject into the continuation's assistant turn so
	// the user sees what was auto-approved at the top of the model's
	// response. Multiple notices (one per auto-approved tool_use in
	// a coalesced turn) join with newlines.
	//
	// Both pass results contribute. The first pass carries notices
	// for whatever the gate fired on in the original assistant turn
	// (always present in this branch since we got here on a
	// continuation). The second pass can also fire the gate when
	// the model re-emits POST /api/control/tasks?surface=inline in
	// the continuation response — that second auto-approval falls
	// back to SubstituteWith because recursion is capped at depth=1,
	// but its notice still belongs in the visible turn for parity
	// with the first task. Without this, two auto-approved tasks in
	// the same inbound request would render with only the first
	// notice and silently elide the second.
	var notices []string
	seen := map[string]struct{}{}
	collect := func(decs []conversation.ToolUseDecisionRecord) {
		for _, dec := range decs {
			n := strings.TrimSpace(dec.Verdict.PrependAssistantNotice)
			if n == "" {
				continue
			}
			if _, ok := seen[n]; ok {
				continue
			}
			seen[n] = struct{}{}
			notices = append(notices, n)
		}
	}
	collect(processed.Decisions)
	collect(newProcessed.Decisions)
	if len(notices) > 0 && len(newProcessed.Body) > 0 {
		joined := strings.Join(notices, "\n")
		pre, changed, prependErr := llmproxy.PrependAssistantNotice(provider, contCT, newProcessed.Body, joined)
		switch {
		case prependErr != nil:
			// Prepend is UX polish, not correctness — log and return
			// the unmodified body so the user still sees the model's
			// output. The continuation itself succeeded.
			h.Logger.WarnContext(r.Context(), "lite-proxy continuation notice prepend failed; returning unannotated body",
				"request_id", requestID, "agent_id", agent.ID, "err", prependErr.Error())
		case !changed:
			// Prepend was a no-op: the dispatcher couldn't find a
			// shape it recognized (response body lacked the expected
			// `choices`/`output`/`content` marker, or Anthropic SSE
			// was missing `message_start`). The audit row for the
			// auto-approval still fired, but the only user-facing
			// trace is gone. Warn so an operator chasing "I
			// auto-approved but the user didn't see the notice"
			// has a deterministic log entry.
			h.Logger.WarnContext(r.Context(), "lite-proxy continuation notice prepend silently no-op'd (shape not recognized); user will not see auto-approval notice",
				"request_id", requestID,
				"agent_id", agent.ID,
				"provider", string(provider),
				"content_type", contCT,
				"body_bytes", len(newProcessed.Body),
			)
		default:
			newProcessed.Body = pre
		}
	}

	return &newProcessed, resp.StatusCode, contCT, contUsage, nil
}

type flushingWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func (fw *flushingWriter) Write(p []byte) (n int, err error) {
	n, err = fw.w.Write(p)
	if fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, err
}

func newFlushingWriter(w io.Writer) io.Writer {
	flusher, _ := w.(http.Flusher)
	return &flushingWriter{w: w, flusher: flusher}
}

type delayedHeaderWriter struct {
	w      http.ResponseWriter
	status int
	wrote  bool
}

func newDelayedHeaderWriter(w http.ResponseWriter, status int) *delayedHeaderWriter {
	return &delayedHeaderWriter{w: w, status: status}
}

func (w *delayedHeaderWriter) Write(p []byte) (int, error) {
	w.Commit()
	return w.w.Write(p)
}

func (w *delayedHeaderWriter) Flush() {
	w.Commit()
	if flusher, ok := w.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *delayedHeaderWriter) Commit() {
	if w.wrote {
		return
	}
	w.wrote = true
	w.w.WriteHeader(w.status)
}

func (w *delayedHeaderWriter) WroteHeader() bool {
	return w != nil && w.wrote
}

func clearHeaders(h http.Header) {
	for k := range h {
		delete(h, k)
	}
}

type streamingTimingWriter struct {
	w                 io.Writer
	logger            *slog.Logger
	requestID         string
	provider          string
	start             time.Time
	upstreamHeadersMs int64
	mu                sync.Mutex
	sawWrite          bool
	sawTextStart      bool
	sawTextDelta      bool
	eventBuf          string
}

func newStreamingTimingWriter(w io.Writer, logger *slog.Logger, requestID, provider string, upstreamHeadersMs int64) io.Writer {
	return &streamingTimingWriter{
		w:                 w,
		logger:            logger,
		requestID:         requestID,
		provider:          provider,
		start:             time.Now(),
		upstreamHeadersMs: upstreamHeadersMs,
	}
}

func (tw *streamingTimingWriter) Write(p []byte) (int, error) {
	n, err := tw.w.Write(p)
	tw.observe(p)
	return n, err
}

func (tw *streamingTimingWriter) observe(p []byte) {
	if tw == nil || tw.logger == nil {
		return
	}
	tw.mu.Lock()
	defer tw.mu.Unlock()

	elapsedMs := time.Since(tw.start).Milliseconds()
	if !tw.sawWrite {
		tw.sawWrite = true
		tw.logger.InfoContext(context.Background(), "lite-proxy stream forwarded first SSE bytes",
			"request_id", tw.requestID,
			"provider", tw.provider,
			"elapsed_ms", elapsedMs,
			"ttfb_headers_ms", tw.upstreamHeadersMs,
			"chunk_bytes", len(p),
		)
	}
	tw.observeSSETextEvents(p, elapsedMs)
}

func (tw *streamingTimingWriter) observeSSETextEvents(p []byte, elapsedMs int64) {
	tw.eventBuf += strings.ReplaceAll(string(p), "\r\n", "\n")
	for {
		idx := strings.Index(tw.eventBuf, "\n\n")
		if idx < 0 {
			if len(tw.eventBuf) > 64<<10 {
				tw.eventBuf = tw.eventBuf[len(tw.eventBuf)-(32<<10):]
			}
			return
		}
		rawEvent := tw.eventBuf[:idx]
		tw.eventBuf = tw.eventBuf[idx+2:]
		var eventName string
		var dataLines []string
		for _, line := range strings.Split(rawEvent, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(dataLines) == 0 {
			continue
		}
		data := strings.Join(dataLines, "\n")
		if !tw.sawTextStart && sseEventIsTextStart(eventName, data) {
			tw.sawTextStart = true
			tw.logger.InfoContext(context.Background(), "lite-proxy stream forwarded text block start",
				"request_id", tw.requestID,
				"provider", tw.provider,
				"elapsed_ms", elapsedMs,
				"ttfb_headers_ms", tw.upstreamHeadersMs,
			)
		}
		if !tw.sawTextDelta && sseEventIsTextDelta(eventName, data) {
			tw.sawTextDelta = true
			tw.logger.InfoContext(context.Background(), "lite-proxy stream forwarded first text delta",
				"request_id", tw.requestID,
				"provider", tw.provider,
				"elapsed_ms", elapsedMs,
				"ttfb_headers_ms", tw.upstreamHeadersMs,
			)
		}
	}
}

func sseEventIsTextStart(eventName, data string) bool {
	switch eventName {
	case "content_block_start":
		var payload struct {
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		return json.Unmarshal([]byte(data), &payload) == nil && payload.ContentBlock.Type == "text"
	case "response.content_part.added":
		var payload struct {
			Part struct {
				Type string `json:"type"`
			} `json:"part"`
		}
		return json.Unmarshal([]byte(data), &payload) == nil && payload.Part.Type == "output_text"
	}
	return false
}

func sseEventIsTextDelta(eventName, data string) bool {
	switch eventName {
	case "content_block_delta":
		var payload struct {
			Delta struct {
				Type string `json:"type"`
			} `json:"delta"`
		}
		return json.Unmarshal([]byte(data), &payload) == nil && payload.Delta.Type == "text_delta"
	case "response.output_text.delta":
		return true
	case "":
		var payload struct {
			Choices []struct {
				Delta struct {
					Content any `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &payload) != nil {
			return false
		}
		for _, choice := range payload.Choices {
			if choice.Delta.Content != nil {
				return true
			}
		}
	}
	return false
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

// pricingModelMatch reports whether two model identifiers resolve to
// the same pricing-table row (case, vendor prefix, date suffix, etc.
// are all collapsed by pricing.Normalize). Used to gate the
// continuation token-merge so a continuation that resolves to a
// different upstream model can't get billed at the first call's rate.
// Returns false when either side is empty — without a known model we
// can't prove a match, and silently merging unattributable tokens
// risks mispricing if a future caller starts populating only one
// side (the existing extractor path leaves Model empty only on
// pathological responses that fail to surface a model at all).
func pricingModelMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return pricing.Normalize(a) == pricing.Normalize(b)
}

func mergeAuditUsage(dst **llmproxy.ExtractUsageResult, auditParams map[string]any, next *llmproxy.ExtractUsageResult, ctx context.Context, logger *slog.Logger, requestID string, agentID string, label string) {
	if next == nil {
		return
	}
	if *dst == nil {
		*dst = next
		auditParams[label+"_usage_recorded"] = true
		return
	}
	if pricingModelMatch((*dst).Model, next.Model) {
		(*dst).Usage.InputTokens += next.Usage.InputTokens
		(*dst).Usage.OutputTokens += next.Usage.OutputTokens
		(*dst).Usage.CacheReadTokens += next.Usage.CacheReadTokens
		(*dst).Usage.CacheWriteTokens += next.Usage.CacheWriteTokens
		(*dst).Usage.CacheWrite1hTokens += next.Usage.CacheWrite1hTokens
		auditParams[label+"_usage_recorded"] = true
		return
	}
	if logger != nil {
		logger.WarnContext(ctx, "lite-proxy usage dropped: model mismatch",
			"request_id", requestID,
			"agent_id", agentID,
			"label", label,
			"existing_model", (*dst).Model,
			"dropped_model", next.Model,
		)
	}
	auditParams[label+"_usage_dropped_model_mismatch"] = true
}

// spliceStreamingContinuationBody only consumes SSE bodies produced by
// Clawvisor's own continuation rewriters. Those helpers JSON-encode every
// data payload, so raw "\n\n" event separators cannot appear inside data.
func spliceStreamingContinuationBody(provider conversation.Provider, result conversation.StreamingRewriteResult, contentType string, body []byte) ([]byte, error) {
	if !conversation.IsSSEContentType(contentType) {
		return body, nil
	}
	switch provider {
	case conversation.ProviderAnthropic:
		return spliceAnthropicContinuationSSE(result.NextAnthropicContentIndex, body)
	case conversation.ProviderOpenAI:
		if result.StreamFormat == "openai_responses" {
			return spliceOpenAIResponsesContinuationSSE(result.StreamID, result.NextOpenAIOutputIndex, body)
		}
		return body, nil
	default:
		return body, nil
	}
}

type liteProxySSEEvent struct {
	event string
	data  string
}

func parseLiteProxySSE(body []byte) []liteProxySSEEvent {
	normalized := strings.ReplaceAll(string(body), "\r\n", "\n")
	blocks := strings.Split(normalized, "\n\n")
	events := make([]liteProxySSEEvent, 0, len(blocks))
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev liteProxySSEEvent
		var dataLines []string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "event:"):
				ev.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(dataLines) == 0 {
			continue
		}
		ev.data = strings.Join(dataLines, "\n")
		events = append(events, ev)
	}
	return events
}

func appendLiteProxySSEEvent(b *bytes.Buffer, event string, payload any) error {
	var data []byte
	switch v := payload.(type) {
	case string:
		data = []byte(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		data = raw
	}
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	return nil
}

func spliceAnthropicContinuationSSE(indexOffset int, body []byte) ([]byte, error) {
	var out bytes.Buffer
	for _, ev := range parseLiteProxySSE(body) {
		if ev.data == "[DONE]" || ev.event == "message_start" {
			continue
		}
		switch ev.event {
		case "content_block_start", "content_block_delta", "content_block_stop":
			var payload map[string]any
			if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
				return nil, err
			}
			if idx, ok := numericJSONIndex(payload["index"]); ok {
				payload["index"] = idx + indexOffset
			}
			if err := appendLiteProxySSEEvent(&out, ev.event, payload); err != nil {
				return nil, err
			}
		default:
			if err := appendLiteProxySSEEvent(&out, ev.event, ev.data); err != nil {
				return nil, err
			}
		}
	}
	return out.Bytes(), nil
}

func spliceOpenAIResponsesContinuationSSE(responseID string, outputIndexOffset int, body []byte) ([]byte, error) {
	var out bytes.Buffer
	for _, ev := range parseLiteProxySSE(body) {
		if ev.data == "[DONE]" || ev.event == "response.created" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
			return nil, err
		}
		if idx, ok := numericJSONIndex(payload["output_index"]); ok {
			payload["output_index"] = idx + outputIndexOffset
		}
		if ev.event == "response.completed" && responseID != "" {
			if resp, ok := payload["response"].(map[string]any); ok {
				resp["id"] = responseID
			}
		}
		if err := appendLiteProxySSEEvent(&out, ev.event, payload); err != nil {
			return nil, err
		}
	}
	out.WriteString("data: [DONE]\n\n")
	return out.Bytes(), nil
}

func numericJSONIndex(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
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
		upstreamPath := llmproxy.ProviderPath(provider, r.URL.Path)
		if u, urlErr := h.Forwarder.Upstream.URL(provider, upstreamPath); urlErr == nil {
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
			code, outcome, message := upstreamCredMissingError(r, agent, provider, h.DashboardBaseURL)
			*auditOutcome = outcome
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusUnauthorized, code, message)
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
		h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusBadGateway, "UPSTREAM_ERROR", "couldn't reach the upstream provider. Please retry; if this persists, check the Clawvisor daemon logs.")
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
	// Capture the response body whenever the upstream returned an error
	// status so we can surface its content in slog (forensic visibility
	// without needing RawIOLogger turned on). On 2xx, only capture if
	// RawIOLogger is configured to avoid buffering large streamed bodies
	// for no reason.
	captureForError := resp.StatusCode >= 400
	if h.RawIOLogger != nil || captureForError {
		capture := newLimitedCaptureWriter(h.MaxResponseBytes)
		w.WriteHeader(resp.StatusCode)
		_, copyErr := io.Copy(io.MultiWriter(w, capture), resp.Body)
		if copyErr != nil {
			*auditOutcome = "upstream_read_error"
			*auditReason = copyErr.Error()
		}
		full := capture.Bytes()
		if captureForError {
			h.Logger.WarnContext(r.Context(), "lite-proxy passthrough upstream error body",
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
		}
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (h *LLMEndpointHandler) preprocessLiteSecretBody(w http.ResponseWriter, r *http.Request, agent *store.Agent, provider conversation.Provider, requestID string, body []byte, extraSuppressed map[string]struct{}, liteProxySecretDetectionDisabled bool, auditParams map[string]any, auditStatus *int, auditDecide, auditOutcome, auditReason *string) ([]byte, bool) {
	// Eleventh migrated call site through pipeline: SecretHistoryStrip.
	// The policy emits secret_history_stripped:true on rewrite; for
	// compatibility with the legacy audit-flag name, we map it back to
	// secret_decision_history_stripped at the handler boundary.
	{
		preBodySHA := liteSecretBodySHA(body)
		pipeReq := &pipelineReadOnlyRequest{
			provider: provider,
			httpReq:  r,
			body:     body,
			userID:   agent.UserID,
			agentID:  agent.ID,
		}
		result, err := runSinglePolicy(r.Context(), pipeReq, policies.NewSecretHistoryStrip())
		if err != nil {
			h.emitLiteSecretPipelineTrace(requestID, agent, "history_strip_error", map[string]any{
				"provider": string(provider),
				"body_sha": preBodySHA,
				"err":      err.Error(),
			})
			h.Logger.WarnContext(r.Context(), "lite-proxy secret decision history strip pipeline failed",
				"agent_id", agent.ID, "err", err.Error())
		} else if string(result.FinalBody) != string(body) {
			h.emitLiteSecretPipelineTrace(requestID, agent, "history_stripped", map[string]any{
				"provider":        string(provider),
				"body_sha_before": preBodySHA,
				"body_sha_after":  liteSecretBodySHA(result.FinalBody),
				"body_bytes":      len(result.FinalBody),
			})
			body = result.FinalBody
			// Keep the legacy audit-flag name for dashboard compatibility.
			auditParams["secret_decision_history_stripped"] = true
		}
	}
	if liteProxySecretDetectionDisabled {
		h.emitLiteSecretPipelineTrace(requestID, agent, "lite_proxy_secret_detection_skipped", map[string]any{
			"provider": string(provider),
			"body_sha": liteSecretBodySHA(body),
			"reason":   "agent_setting",
		})
		return body, false
	}
	// Ninth migrated call site through pipeline: SecretRewrites.
	{
		pipeReq := &pipelineReadOnlyRequest{
			provider: provider,
			httpReq:  r,
			body:     body,
			userID:   agent.UserID,
			agentID:  agent.ID,
		}
		resolver := func(ctx context.Context, b []byte) ([]byte, bool) {
			return h.applyRememberedSecretRewrites(ctx, agent, provider, requestID, b)
		}
		result, err := runSinglePolicy(r.Context(), pipeReq, policies.NewSecretRewrites(resolver))
		if err != nil {
			h.Logger.WarnContext(r.Context(), "lite-proxy secret rewrites pipeline failed",
				"agent_id", agent.ID, "err", err.Error())
		} else {
			body = result.FinalBody
			for k, v := range result.AuditFields {
				auditParams[k] = v
			}
		}
	}
	// Tenth migrated call site through pipeline: SecretHold.
	pipeReq := &pipelineReadOnlyRequest{
		provider: provider,
		httpReq:  r,
		body:     body,
		userID:   agent.UserID,
		agentID:  agent.ID,
	}
	resolver := func(_ context.Context, b []byte) policies.SecretHoldResult {
		out := h.tryHoldInboundSecret(r, agent, provider, requestID, b, extraSuppressed)
		if !out.Held {
			return policies.SecretHoldResult{Held: false}
		}
		fields := map[string]any{}
		if out.DecisionID != "" {
			fields["secret_decision_id"] = out.DecisionID
		}
		if len(out.Findings) > 0 {
			fields["secret_findings"] = len(out.Findings)
			fields["secret_suggested_name"] = out.Findings[0].SuggestedName
			fields["secret_sources"] = liteSecretFindingSources(out.Findings)
		}
		// IsError → handler writes a writeLiteProxyError-shaped response,
		// not a 200 hold prompt. Surface the error info via the policy
		// AuditFields so the handler can branch on it.
		if out.IsError {
			fields["secret_hold_is_error"] = true
			fields["secret_hold_error_code"] = out.ErrorCode
			fields["secret_hold_error_message"] = out.ErrorMessage
		}
		return policies.SecretHoldResult{
			Held:        true,
			HTTPStatus:  out.HTTPStatus,
			Body:        out.Body,
			ContentType: out.ContentType,
			Decision:    out.Decision,
			Outcome:     out.Outcome,
			Reason:      out.Reason,
			AuditFields: fields,
		}
	}
	holdResult, err := runSinglePolicy(r.Context(), pipeReq, policies.NewSecretHold(resolver))
	if err != nil {
		h.Logger.WarnContext(r.Context(), "lite-proxy secret hold pipeline failed",
			"agent_id", agent.ID, "err", err.Error())
		return body, false
	}
	for k, v := range holdResult.AuditFields {
		auditParams[k] = v
	}
	if sc := holdResult.ShortCircuit; sc != nil {
		*auditStatus = sc.StatusCode
		if v, _ := holdResult.AuditFields["secret_hold_outcome"].(string); v != "" {
			*auditOutcome = v
		}
		if v, _ := holdResult.AuditFields["secret_hold_decision"].(string); v != "" {
			*auditDecide = v
		}
		if v, _ := holdResult.AuditFields["secret_hold_reason"].(string); v != "" {
			*auditReason = v
		}
		// Error path: writeLiteProxyError formatting.
		if isErr, _ := holdResult.AuditFields["secret_hold_is_error"].(bool); isErr {
			code, _ := holdResult.AuditFields["secret_hold_error_code"].(string)
			msg, _ := holdResult.AuditFields["secret_hold_error_message"].(string)
			h.writeLiteProxyError(w, r, agent, provider, body, requestID, sc.StatusCode, code, msg)
			return body, true
		}
		// Success path: 200 with synthesized hold prompt.
		for k, v := range sc.Headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(sc.StatusCode)
		_, _ = w.Write(sc.Body)
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
					h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusConflict, "SECRET_VAULT_NAME_CONFLICT", "a vault item with that name already exists with a different value. Please choose a different name and retry.")
				} else {
					*auditStatus = http.StatusInternalServerError
					*auditOutcome = "secret_vault_failed"
					h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusInternalServerError, "SECRET_VAULT_FAILED", "couldn't save the detected secret to your vault. Please retry.")
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
				h.writeLiteProxyError(w, r, agent, provider, body, requestID, http.StatusInternalServerError, "SECRET_VAULT_FAILED", "couldn't substitute the detected secret into the request. Please retry.")
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

// liteSecretHoldOutcome is the result-returning shape of the
// secret-hold check. Used by both the legacy maybeHoldInboundSecret
// method (which writes the response from this struct) and the
// SecretHold pipeline policy (which converts it into a ShortCircuit
// verdict).
type liteSecretHoldOutcome struct {
	Held        bool
	HTTPStatus  int
	Body        []byte
	ContentType string
	Decision    string
	Outcome     string
	Reason      string
	DecisionID  string
	Findings    []llmproxy.InboundSecretFinding
	// IsError flags whether the hold attempt itself failed (vs.
	// successfully created a hold). Errors map to writeLiteProxyError
	// with SECRET_HOLD_FAILED; successful holds map to the synthetic
	// hold-prompt response.
	IsError       bool
	ErrorCode     string
	ErrorMessage  string
}

// tryHoldInboundSecret runs the secret-hold scan and adjudication
// without writing to the response. Returns liteSecretHoldOutcome
// which the caller maps to either a written response or a pipeline
// ShortCircuit verdict.
func (h *LLMEndpointHandler) tryHoldInboundSecret(r *http.Request, agent *store.Agent, provider conversation.Provider, requestID string, body []byte, extraSuppressed map[string]struct{}) liteSecretHoldOutcome {
	if h == nil || h.PendingSecrets == nil || agent == nil {
		return liteSecretHoldOutcome{Held: false}
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
			"request_id", requestID, "agent_id", agent.ID, "err", err.Error())
		return liteSecretHoldOutcome{Held: false}
	}
	if !found {
		h.emitLiteSecretPipelineTrace(requestID, agent, "hold_scan_no_findings", map[string]any{
			"provider":      string(provider),
			"adjudications": liteSecretAdjudicationTraceSummaries(scan.Adjudications),
			"body_sha":      liteSecretBodySHA(body),
		})
		return liteSecretHoldOutcome{Held: false}
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
		return liteSecretHoldOutcome{
			Held:         true,
			HTTPStatus:   http.StatusInternalServerError,
			Decision:     "deny",
			Outcome:      "secret_hold_failed",
			Reason:       err.Error(),
			IsError:      true,
			ErrorCode:    "SECRET_HOLD_FAILED",
			ErrorMessage: "couldn't pause this request for secret review. Please retry.",
		}
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
	prompt := renderInboundSecretPrompt(held)
	bodyBytes, contentType := syntheticLiteTextResponse(r, provider, body, prompt)
	return liteSecretHoldOutcome{
		Held:        true,
		HTTPStatus:  http.StatusOK,
		Body:        bodyBytes,
		ContentType: contentType,
		Decision:    "block",
		Outcome:     "secret_detected",
		Reason:      "raw secret detected in inbound LLM request",
		DecisionID:  held.ID,
		Findings:    scan.Findings,
	}
}

func (h *LLMEndpointHandler) maybeHoldInboundSecret(w http.ResponseWriter, r *http.Request, agent *store.Agent, provider conversation.Provider, requestID string, body []byte, extraSuppressed map[string]struct{}, auditParams map[string]any, auditStatus *int, auditDecide, auditOutcome, auditReason *string) bool {
	outcome := h.tryHoldInboundSecret(r, agent, provider, requestID, body, extraSuppressed)
	if !outcome.Held {
		return false
	}
	*auditStatus = outcome.HTTPStatus
	*auditDecide = outcome.Decision
	*auditOutcome = outcome.Outcome
	*auditReason = outcome.Reason
	if outcome.IsError {
		h.writeLiteProxyError(w, r, agent, provider, body, requestID, outcome.HTTPStatus, outcome.ErrorCode, outcome.ErrorMessage)
		return true
	}
	if auditParams != nil {
		auditParams["secret_decision_id"] = outcome.DecisionID
		auditParams["secret_findings"] = len(outcome.Findings)
		if len(outcome.Findings) > 0 {
			auditParams["secret_suggested_name"] = outcome.Findings[0].SuggestedName
			auditParams["secret_sources"] = liteSecretFindingSources(outcome.Findings)
		}
	}
	w.Header().Set("Content-Type", outcome.ContentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(outcome.HTTPStatus)
	_, _ = w.Write(outcome.Body)
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

// tryApprovalRelease runs the release attempt and returns the result
// without writing to the response. Used by both the legacy
// maybeHandleLiteApprovalRelease method and the policies.ApprovalRelease
// pipeline policy — the latter calls this via a constructor closure.
//
// Returns a sentinel ReleaseResult with HTTPStatus=503 +
// Outcome=decision_input_load_failed when the decision-input load
// fails; the caller maps that to a 503 response.
func (h *LLMEndpointHandler) tryApprovalRelease(r *http.Request, agent *store.Agent, provider conversation.Provider, requestID, conversationID string, body []byte) llmproxy.ReleaseResult {
	candidateTasks, toolRules, egressRules, decisionLoadErr := h.loadLiteProxyDecisionInputs(r.Context(), agent)
	if decisionLoadErr != nil {
		return llmproxy.ReleaseResult{
			Handled:    true,
			HTTPStatus: http.StatusServiceUnavailable,
			Decision:   "deny",
			Outcome:    "decision_input_load_failed",
			Reason:     decisionLoadErr.Error(),
		}
	}
	var catalogIface interface {
		Resolve(host, method, path string) (llmproxy.ResolvedAction, bool)
	}
	if h.Catalog != nil {
		catalogIface = h.Catalog
	}
	opts := inspector.DefaultRewriteOpts(h.ResolverBaseURL)
	opts.CallerToken = inboundAgentToken(r)
	return llmproxy.TryReleasePendingApproval(r.Context(), llmproxy.ReleaseRequest{
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
}

func (h *LLMEndpointHandler) maybeHandleLiteApprovalRelease(w http.ResponseWriter, r *http.Request, agent *store.Agent, provider conversation.Provider, requestID, conversationID string, body []byte, auditStatus *int, auditDecide, auditOutcome, auditReason *string) bool {
	result := h.tryApprovalRelease(r, agent, provider, requestID, conversationID, body)
	if result.Outcome == "decision_input_load_failed" {
		h.Logger.WarnContext(r.Context(), "lite-proxy approval-release decision-input load failed; failing closed",
			"request_id", requestID, "agent_id", agent.ID, "err", result.Reason)
		*auditStatus = result.HTTPStatus
		*auditDecide = result.Decision
		*auditOutcome = result.Outcome
		*auditReason = result.Reason
		h.writeLiteProxyError(w, r, agent, provider, body, requestID, result.HTTPStatus, "DECISION_INPUT_UNAVAILABLE",
			"couldn't load authorization data right now. Please retry in a moment.")
		return true
	}
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
		h.writeLiteProxyError(w, r, agent, provider, body, requestID, result.HTTPStatus, "APPROVAL_RELEASE_ERROR", result.Reason)
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

// liteProxyConversationIDSource labels how the conversation ID currently
// in use was derived, for the per-request audit row. Lets dashboards
// answer "what fraction of Chat-Completions turn-2+ requests had the
// minted marker round-trip?" without re-doing the lookup. Values are
// stable strings safe to switch on in downstream queries.
func liteProxyConversationIDSource(provider conversation.Provider, req *http.Request, conversationID, mintedConversationID string) string {
	if conversationID == "" {
		return "empty"
	}
	if mintedConversationID != "" && conversationID == mintedConversationID {
		return "minted"
	}
	switch provider {
	case conversation.ProviderAnthropic:
		return "native_anthropic"
	case conversation.ProviderOpenAI:
		if req != nil && conversation.IsOpenAIChatCompletionsEndpoint(req) {
			if strings.HasPrefix(conversationID, conversation.ConversationIDPrefix) {
				return "echoed_marker"
			}
			if strings.HasPrefix(conversationID, "fp-") {
				return "fingerprint"
			}
			return "unknown"
		}
		return "native_openai_responses"
	}
	return "unknown"
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
