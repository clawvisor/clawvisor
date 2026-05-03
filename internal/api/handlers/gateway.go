package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/callback"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/intent"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/gateway"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// pendingRequestBlob is stored in pending_approvals.request_blob.
// It contains everything needed to re-execute the request on approval.
type pendingRequestBlob struct {
	Service      string                      `json:"service"`
	Action       string                      `json:"action"`
	Params       map[string]any              `json:"params"`
	UserID       string                      `json:"user_id"`
	AgentID      string                      `json:"agent_id"`
	AgentName    string                      `json:"agent_name"`
	RequestID    string                      `json:"request_id"`
	TaskID       string                      `json:"task_id"`
	Reason       string                      `json:"reason"`
	CallbackURL  string                      `json:"callback_url"`
	Verification *intent.VerificationVerdict `json:"verification,omitempty"`
}

// GatewayHooks allows cloud/enterprise layers to inject additional
// authorization logic into the gateway request flow.
type GatewayHooks struct {
	// BeforeAuthorize is called after request parsing, before restriction checks.
	// Return a non-nil error to block the request.
	BeforeAuthorize func(ctx context.Context, agentID, userID, service, action string) error
}

// LocalServiceExecutor handles execution of local daemon service requests.
// Implemented by the cloud layer; nil in self-hosted mode.
type LocalServiceExecutor interface {
	// Execute forwards a request to the appropriate local daemon.
	// The service should include the "local." prefix (e.g. "local.my_service").
	// The caller has already enforced restrictions and task scope.
	Execute(ctx context.Context, userID, service, action string, params map[string]any) (*adapters.Result, error)
}

// isLocalService returns true for services provided by local daemons.
func isLocalService(serviceType string) bool {
	return strings.HasPrefix(serviceType, "local.")
}

// GatewayHandler handles POST /api/gateway/request.
type GatewayHandler struct {
	store            store.Store
	vault            vault.Vault
	adapterReg       *adapters.Registry
	notifier         notify.Notifier // may be nil if Telegram not configured
	verifier         intent.Verifier
	extractor        intent.Extractor
	extractTrack     ExtractionTracker // tracks in-flight async extractions; never nil
	cfg              config.Config
	logger           *slog.Logger
	baseURL          string
	eventHub         events.EventHub
	requestResolver  runtimepolicy.GatewayRequestResolver
	gatewayHooks     *GatewayHooks        // cloud-injected authorization hooks; may be nil
	localExec        LocalServiceExecutor // cloud-injected local service executor; may be nil
	localSvcProvider LocalServiceProvider // cloud-injected local service catalog; may be nil
}

func NewGatewayHandler(
	st store.Store,
	v vault.Vault,
	adapterReg *adapters.Registry,
	notifier notify.Notifier,
	verifier intent.Verifier,
	extractor intent.Extractor,
	cfg config.Config,
	logger *slog.Logger,
	baseURL string,
	eventHub events.EventHub,
) *GatewayHandler {
	return &GatewayHandler{
		store: st, vault: v, adapterReg: adapterReg,
		notifier: notifier, verifier: verifier, extractor: extractor,
		extractTrack: NewMemoryExtractionTracker(60 * time.Second),
		cfg:          cfg, logger: logger, baseURL: baseURL,
		eventHub: eventHub,
	}
}

// SetExtractionTracker overrides the default in-memory tracker. Use the
// Redis-backed tracker in multi-instance deployments so that a request
// arriving on server B can see that server A is still extracting.
func (h *GatewayHandler) SetExtractionTracker(t ExtractionTracker) {
	if t != nil {
		h.extractTrack = t
	}
}

// SetGatewayHooks configures cloud-injected authorization hooks.
// Must be called before any requests are handled.
func (h *GatewayHandler) SetGatewayHooks(hooks *GatewayHooks) {
	h.gatewayHooks = hooks
}

func (h *GatewayHandler) SetGatewayRequestResolver(resolver runtimepolicy.GatewayRequestResolver) {
	h.requestResolver = resolver
}

// SetLocalServiceExecutor configures the local daemon service executor.
func (h *GatewayHandler) SetLocalServiceExecutor(e LocalServiceExecutor) {
	h.localExec = e
}

// SetLocalServiceProvider configures the local service catalog provider for
// request-time action validation.
func (h *GatewayHandler) SetLocalServiceProvider(p LocalServiceProvider) {
	h.localSvcProvider = p
}

// HandleRequest is the main gateway entry point.
//
// Authorization flow: restrictions → task scope → per-request approval.
//
// POST /api/gateway/request
// Auth: agent bearer token
func (h *GatewayHandler) HandleRequest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()

	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var req gateway.Request
	if !decodeJSON(w, r, &req) {
		return
	}

	// Collect missing pre-restriction required fields (service, action, reason).
	// task_id is checked after restriction checks — restrictions can block requests
	// before a task is created, so task_id is intentionally not required here.
	{
		var missing []string
		if req.Service == "" {
			missing = append(missing, "service")
		}
		if req.Action == "" {
			missing = append(missing, "action")
		}
		if req.Reason == "" {
			missing = append(missing, "reason")
		}
		if len(missing) > 0 {
			code := "INVALID_REQUEST"
			if len(missing) == 1 && missing[0] == "reason" {
				code = "MISSING_REASON"
			}
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error:         "missing required fields: " + strings.Join(missing, ", "),
				Code:          code,
				MissingFields: missing,
				Hint:          "Every gateway request must specify the target service, action, and a reason for the request.",
				Example: map[string]any{
					"service": "google.gmail",
					"action":  "list_messages",
					"reason":  "Fetch recent emails to summarize for the user",
					"task_id": "<task_id from POST /api/tasks>",
					"params":  map[string]any{"max_results": 10},
				},
			})
			return
		}
	}

	middleware.AddLogField(ctx, "service", req.Service)
	middleware.AddLogField(ctx, "action", req.Action)

	if req.Context.CallbackURL != "" {
		if err := callback.ValidateCallbackURL(req.Context.CallbackURL); err != nil {
			h.logger.Warn("callback URL blocked by SSRF policy",
				"callback_url", req.Context.CallbackURL,
				"err", err,
				"agent_id", agent.ID,
			)
			writeError(w, http.StatusBadRequest, "INVALID_CALLBACK_URL", err.Error())
			return
		}
	}

	// Parse alias from service field (e.g. "google.gmail:personal" → type="google.gmail", alias="personal").
	serviceType, serviceAlias := parseServiceAlias(req.Service)

	if req.RequestID == "" {
		req.RequestID = uuid.New().String()
	} else {
		// Dedup: if this request_id was already processed under the same task,
		// return the existing outcome without re-processing. This prevents
		// duplicate audit entries and re-execution of side-effect actions when
		// the agent retries after a network timeout.
		//
		// The dedup is scoped to the same task_id so that reusing a request_id
		// across different tasks/sessions is treated as a fresh request.
		// Pre-task outcomes (e.g. "blocked" by restriction) have no task_id
		// and are always dedup'd since they're stateless checks.
		if existing, err := h.store.GetAuditEntryByRequestID(ctx, req.RequestID, agent.UserID); err == nil {
			preTask := existing.TaskID == nil // blocked/error before task scope
			sameTask := req.TaskID != "" && existing.TaskID != nil && *existing.TaskID == req.TaskID
			if preTask || sameTask {
				writeGatewayStatusResponse(w, existing, gatewayStatusResponseOptions{
					Deduped: true,
					Message: "Duplicate request_id reused; returning the existing result. Use a new request_id for a new request.",
				})
				return
			}
		}
	}
	middleware.AddLogField(ctx, "request_id", req.RequestID)

	paramsSafe, _ := json.Marshal(format.StripSecrets(cloneParams(req.Params)))

	auditID := uuid.New().String()

	// Backup log: append-only record of every gateway request. Written after
	// the response so it captures the outcome. If the primary audit insert is
	// ever silently dropped, this table retains full visibility.
	var outDecision, outOutcome string
	defer func() {
		logCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if logErr := h.store.LogGatewayRequest(logCtx, &store.GatewayRequestLog{
			AuditID:    auditID,
			RequestID:  req.RequestID,
			AgentID:    agent.ID,
			UserID:     agent.UserID,
			Service:    req.Service,
			Action:     req.Action,
			TaskID:     req.TaskID,
			Reason:     req.Reason,
			Decision:   outDecision,
			Outcome:    outOutcome,
			DurationMS: int(time.Since(start).Milliseconds()),
		}); logErr != nil {
			h.logger.Warn("backup request log failed", "err", logErr)
		}
	}()

	// baseEntry builds an AuditEntry with fields common to all outcomes.
	// It also records the decision/outcome for the deferred backup log.
	baseEntry := func(decision, outcome string, taskID *string) *store.AuditEntry {
		outDecision, outOutcome = decision, outcome
		return &store.AuditEntry{
			ID:         auditID,
			UserID:     agent.UserID,
			AgentID:    &agent.ID,
			RequestID:  req.RequestID,
			TaskID:     taskID,
			Timestamp:  time.Now().UTC(),
			Service:    req.Service,
			Action:     req.Action,
			ParamsSafe: json.RawMessage(paramsSafe),
			Decision:   decision,
			Outcome:    outcome,
			Reason:     nullableStr(req.Reason),
			DataOrigin: req.Context.DataOrigin,
			ContextSrc: nullableStr(req.Context.Source),
		}
	}

	// ── Step 0: Cloud gateway hooks (org-level restrictions) ────────────────
	if h.gatewayHooks != nil && h.gatewayHooks.BeforeAuthorize != nil {
		if err := h.gatewayHooks.BeforeAuthorize(ctx, agent.ID, agent.UserID, req.Service, req.Action); err != nil {
			middleware.AddLogField(ctx, "decision", "block")
			middleware.AddLogField(ctx, "outcome", "blocked")
			e := baseEntry("block", "blocked", nil)
			e.DurationMS = int(time.Since(start).Milliseconds())
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			h.publishAuditAndQueue(agent.UserID, "")
			resp := map[string]any{
				"status":     "blocked",
				"request_id": req.RequestID,
				"audit_id":   auditID,
				"code":       gateway.CodeRestricted,
				"reason":     err.Error(),
			}
			h.maybeInjectNPS(ctx, resp, agent.ID)
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}

	// ── Step 1: Check policy blocks ───────────────────────────────────────────
	// Check both migrated runtime policy service rules and legacy restrictions.
	serviceRule, _ := matchServicePolicyRule(ctx, h.store, agent.UserID, req.Service, req.Action)
	if serviceRule == nil && serviceAlias != "default" {
		serviceRule, _ = matchServicePolicyRule(ctx, h.store, agent.UserID, serviceType, req.Action)
	}
	restriction, _ := h.store.MatchRestriction(ctx, agent.UserID, req.Service, req.Action)
	if restriction == nil && serviceAlias != "default" {
		restriction, _ = h.store.MatchRestriction(ctx, agent.UserID, serviceType, req.Action)
	}
	if serviceRule != nil || restriction != nil {
		middleware.AddLogField(ctx, "decision", "block")
		middleware.AddLogField(ctx, "outcome", "blocked")
		e := baseEntry("block", "blocked", nil)
		if serviceRule != nil {
			e.RuleID = &serviceRule.ID
		}
		e.DurationMS = int(time.Since(start).Milliseconds())
		if logErr := h.store.LogAudit(ctx, e); logErr != nil {
			h.logger.Warn("audit log failed", "err", logErr)
		}
		h.publishAuditAndQueue(agent.UserID, "")
		reason := ""
		if serviceRule != nil {
			reason = serviceRule.Reason
		}
		if reason == "" && restriction != nil {
			reason = restriction.Reason
		}
		if reason == "" && restriction != nil {
			reason = fmt.Sprintf("Restricted: %s:%s is blocked", restriction.Service, restriction.Action)
		}
		if reason == "" && serviceRule != nil {
			reason = fmt.Sprintf("Policy blocked: %s:%s", serviceRule.Service, serviceRule.ServiceAction)
		}
		resp := map[string]any{
			"status":     "blocked",
			"request_id": req.RequestID,
			"audit_id":   auditID,
			"code":       gateway.CodeRestricted,
			"reason":     reason,
		}
		h.maybeInjectNPS(ctx, resp, agent.ID)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// ── Step 2: Task context resolution ──────────────────────────────────────
	if req.TaskID == "" {
		// When classification is gated off (the default), a missing task_id is
		// always a 400. Skip the classifier — older clients depend on the
		// TASK_REQUIRED error contract.
		if !h.cfg.Gateway.ClassifyMissingTaskID {
			e := baseEntry("reject", "validation_error", nil)
			e.DurationMS = int(time.Since(start).Milliseconds())
			errMsg := "missing required field: task_id"
			e.ErrorMsg = &errMsg
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error:         errMsg,
				Code:          "TASK_REQUIRED",
				MissingFields: []string{"task_id"},
				Hint:          "Create a task first via POST /api/tasks, then include the returned task_id in every gateway request.",
				Example: map[string]any{
					"service": "google.gmail",
					"action":  "list_messages",
					"reason":  "Fetch recent emails to summarize for the user",
					"task_id": "<task_id from POST /api/tasks>",
				},
			})
			return
		}

		tasks, _, err := h.store.ListTasks(ctx, agent.UserID, store.TaskFilter{ActiveOnly: true})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load active tasks")
			return
		}
		classification := runtimepolicy.ClassifyGatewayRequest(tasks, agent.ID, serviceType, serviceAlias, req.Action)
		if h.requestResolver != nil {
			resolved, resolveErr := h.requestResolver.Resolve(ctx, runtimepolicy.GatewayRequestResolutionRequest{
				Classification: classification,
				ServiceType:    serviceType,
				ServiceAlias:   serviceAlias,
				Action:         req.Action,
				Reason:         req.Reason,
				Params:         req.Params,
			})
			if resolveErr != nil {
				h.logger.Warn("gateway request resolver failed", "err", resolveErr, "service", serviceType, "action", req.Action, "request_id", req.RequestID)
			} else {
				classification = resolved
			}
		}
		switch classification.Kind {
		case runtimepolicy.ClassificationBelongsToExistingTask:
			req.TaskID = classification.MatchedTask.ID
		case runtimepolicy.ClassificationAmbiguous:
			e := baseEntry("review", "blocked", nil)
			e.DurationMS = int(time.Since(start).Milliseconds())
			errMsg := "request matches multiple active tasks; specify task_id explicitly"
			e.ErrorMsg = &errMsg
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			h.publishAuditAndQueue(agent.UserID, "")
			resp := map[string]any{
				"status":          "ambiguous_task",
				"request_id":      req.RequestID,
				"audit_id":        auditID,
				"code":            "TASK_AMBIGUOUS",
				"classification":  classification.Kind,
				"candidate_tasks": summarizeCandidateTasks(classification.CandidateTasks),
				"message":         "This request is covered by multiple active tasks. Provide task_id explicitly or start a task to bias resolution.",
			}
			h.maybeInjectNPS(ctx, resp, agent.ID)
			writeJSON(w, http.StatusConflict, resp)
			return
		case runtimepolicy.ClassificationNeedsNewTask, runtimepolicy.ClassificationOneOff:
			middleware.AddLogField(ctx, "decision", "approve")
			middleware.AddLogField(ctx, "outcome", "pending")
			e := baseEntry("approve", "pending", nil)
			e.DurationMS = int(time.Since(start).Milliseconds())
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			h.publishAuditAndQueue(agent.UserID, "")
			expiresAt := time.Now().Add(time.Duration(h.cfg.Approval.Timeout) * time.Second)
			blob := buildRequestBlob(req, agent)
			reason := "raw request needs review before execution"
			if classification.Kind == runtimepolicy.ClassificationNeedsNewTask {
				reason = "request is outside every active task and may need a new task"
			}
			if routeErr := h.routeToApproval(ctx, agent.UserID, blob, auditID, req.Context.CallbackURL, expiresAt, reason, nil); routeErr != nil {
				h.logger.Warn("route to approval failed", "err", routeErr)
			}
			if r.URL.Query().Get("wait") == "true" && h.eventHub != nil {
				pa := h.waitForApprovalDecision(r.Context(), req.RequestID, agent.UserID, longPollDeadline(r))
				if pa != nil && pa.Status == "approved" && !time.Now().After(pa.ExpiresAt) {
					h.executeAndRespond(w, r.Context(), pa, agent.ID)
					return
				}
				if pa == nil {
					if entry, err := h.store.GetAuditEntryByRequestID(r.Context(), req.RequestID, agent.UserID); err == nil && entry.Outcome != "pending" {
						writeGatewayStatusResponse(w, entry)
						return
					}
				}
			}
			resp := map[string]any{
				"status":         "pending",
				"request_id":     req.RequestID,
				"audit_id":       auditID,
				"classification": classification.Kind,
				"message":        fmt.Sprintf("Approval requested. Waiting up to %ds.", h.cfg.Approval.Timeout),
			}
			h.maybeInjectNPS(ctx, resp, agent.ID)
			writeJSON(w, http.StatusAccepted, resp)
			return
		default:
			e := baseEntry("reject", "validation_error", nil)
			e.DurationMS = int(time.Since(start).Milliseconds())
			errMsg := "missing required field: task_id"
			e.ErrorMsg = &errMsg
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error:         errMsg,
				Code:          "TASK_REQUIRED",
				MissingFields: []string{"task_id"},
				Hint:          "Create a task first via POST /api/tasks, then include the returned task_id in every gateway request.",
				Example: map[string]any{
					"service": "google.gmail",
					"action":  "list_messages",
					"reason":  "Fetch recent emails to summarize for the user",
					"task_id": "<task_id from POST /api/tasks>",
				},
			})
			return
		}
	}

	// ── Step 3: Hardcoded approval check ─────────────────────────────────────
	hardcoded := RequiresHardcodedApproval(serviceType, req.Action)

	// ── Step 4: Task scope enforcement ───────────────────────────────────────
	var advisoryVerdict *intent.VerificationVerdict
	var warnings []string
	{
		task, taskErr := h.store.GetTask(ctx, req.TaskID)
		if taskErr != nil {
			taskIDPtr := &req.TaskID
			e := baseEntry("reject", "validation_error", taskIDPtr)
			e.DurationMS = int(time.Since(start).Milliseconds())
			errMsg := fmt.Sprintf("task %q not found", req.TaskID)
			e.ErrorMsg = &errMsg
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error: errMsg,
				Code:  "INVALID_REQUEST",
				Hint:  "The task_id may be incorrect, or the task may have been deleted. Create a new task via POST /api/tasks and use the returned task_id.",
			})
			return
		}
		if task.UserID != agent.UserID {
			taskIDPtr := &req.TaskID
			e := baseEntry("reject", "forbidden", taskIDPtr)
			e.DurationMS = int(time.Since(start).Milliseconds())
			errMsg := "task does not belong to this agent's user"
			e.ErrorMsg = &errMsg
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			writeDetailedError(w, http.StatusForbidden, apiErrorDetail{
				Error: errMsg,
				Code:  "FORBIDDEN",
				Hint:  "This agent's bearer token is associated with a different user than the task owner. Ensure you are using the correct agent token and task_id.",
			})
			return
		}
		if task.ExpiresAt != nil && time.Now().After(*task.ExpiresAt) {
			taskIDPtr := &req.TaskID
			e := baseEntry("reject", "task_expired", taskIDPtr)
			e.DurationMS = int(time.Since(start).Milliseconds())
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			resp := map[string]any{
				"status":  "task_expired",
				"task_id": req.TaskID,
				"message": "Task has expired. Create a new task via POST /api/tasks, or use POST /api/tasks/{id}/expand to request an extension before expiry.",
			}
			h.maybeInjectNPS(ctx, resp, agent.ID)
			writeJSON(w, http.StatusOK, resp)
			return
		}
		if task.Status != "active" {
			taskIDPtr := &req.TaskID
			hint := "Create a new task via POST /api/tasks."
			switch task.Status {
			case "pending_approval":
				hint = "The task is still waiting for user approval. Poll with GET /api/tasks/{id}?wait=true or wait for the callback."
			case "denied":
				hint = "The user denied this task. Create a new task with a revised purpose/scope."
			case "completed":
				hint = "This task was already marked complete. Create a new task for additional work."
			case "revoked":
				hint = "The user revoked this task. Create a new task if you need to continue."
			case "pending_scope_expansion":
				hint = "A scope expansion is pending approval. Wait for it to be approved before making requests."
			}
			errMsg := fmt.Sprintf("task is %s, not active", task.Status)
			e := baseEntry("reject", "invalid_state", taskIDPtr)
			e.DurationMS = int(time.Since(start).Milliseconds())
			e.ErrorMsg = &errMsg
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			writeDetailedError(w, http.StatusConflict, apiErrorDetail{
				Error: errMsg,
				Code:  "INVALID_STATE",
				Hint:  hint,
			})
			return
		}

		if task.Lifetime == "standing" && req.SessionID == "" {
			taskIDPtr := &req.TaskID
			e := baseEntry("reject", "validation_error", taskIDPtr)
			e.DurationMS = int(time.Since(start).Milliseconds())
			errMsg := "session_id is required for standing task requests"
			e.ErrorMsg = &errMsg
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
				Error: errMsg,
				Code:  "MISSING_SESSION_ID",
				Hint:  "Standing tasks require a session_id on every gateway request to enable chain context verification. Generate a UUID per workflow invocation and pass it as session_id.",
			})
			return
		}

		// Check whether the action exists on the adapter before checking task scope.
		// This prevents confusing "out of scope" errors when the real issue is a
		// non-existent action (e.g. calling search_messages on Gmail instead of list_messages).
		if adp, adpOK := h.adapterReg.GetForUser(ctx, serviceType, agent.UserID); adpOK {
			supported := adp.SupportedActions()
			found := false
			for _, a := range supported {
				if a == req.Action {
					found = true
					break
				}
			}
			if !found {
				_ = h.store.IncrementTaskRequestCount(ctx, req.TaskID)
				msg := fmt.Sprintf(
					"Action %q does not exist on service %s. Available actions: %s",
					req.Action, serviceType, strings.Join(supported, ", "),
				)
				taskIDPtr := &req.TaskID
				e := baseEntry("unknown_action", "blocked", taskIDPtr)
				e.DurationMS = int(time.Since(start).Milliseconds())
				e.ErrorMsg = &msg
				if logErr := h.store.LogAudit(ctx, e); logErr != nil {
					h.logger.Warn("audit log failed", "err", logErr)
				}
				h.publishAuditAndQueue(agent.UserID, req.TaskID)
				writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
					Error: msg,
					Code:  "UNKNOWN_ACTION",
					Hint:  fmt.Sprintf("This service does not have a %q action. Check the available actions listed above and use the correct one.", req.Action),
				})
				return
			}
		} else if isLocalService(serviceType) && h.localSvcProvider != nil {
			// Validate action exists on the local service (mirrors cloud adapter check above).
			if localErr := h.validateLocalAction(ctx, agent.UserID, serviceType, req.Action); localErr != nil {
				_ = h.store.IncrementTaskRequestCount(ctx, req.TaskID)
				msg := localErr.Error()
				taskIDPtr := &req.TaskID
				e := baseEntry("unknown_action", "blocked", taskIDPtr)
				e.DurationMS = int(time.Since(start).Milliseconds())
				e.ErrorMsg = &msg
				if logErr := h.store.LogAudit(ctx, e); logErr != nil {
					h.logger.Warn("audit log failed", "err", logErr)
				}
				h.publishAuditAndQueue(agent.UserID, req.TaskID)
				writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
					Error: msg,
					Code:  "UNKNOWN_ACTION",
					Hint:  fmt.Sprintf("This local service does not have a %q action. Check the available actions listed above and use the correct one.", req.Action),
				})
				return
			}
		}

		match := CheckTaskScope(task, serviceType, serviceAlias, req.Action)

		if !match.InScope {
			_ = h.store.IncrementTaskRequestCount(ctx, req.TaskID)
			msg := fmt.Sprintf("Action %s:%s is outside the approved task scope. Use POST /api/tasks/%s/expand to request it.",
				req.Service, req.Action, req.TaskID)
			if task.Lifetime == "standing" {
				msg = fmt.Sprintf("Action %s:%s is outside this standing task's scope. Standing tasks cannot be expanded — create a separate session task for this action, or revoke this task and create a new one with the additional actions.",
					req.Service, req.Action)
			}
			taskIDPtr := &req.TaskID
			e := baseEntry("out_of_scope", "blocked", taskIDPtr)
			e.DurationMS = int(time.Since(start).Milliseconds())
			e.ErrorMsg = &msg
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			h.publishAuditAndQueue(agent.UserID, req.TaskID)
			resp := map[string]any{
				"status":     "pending_scope_expansion",
				"code":       gateway.CodeScopeMismatch,
				"task_id":    req.TaskID,
				"request_id": req.RequestID,
				"audit_id":   auditID,
				"message":    msg,
			}
			h.maybeInjectNPS(ctx, resp, agent.ID)
			writeJSON(w, http.StatusOK, resp)
			return
		}

		_ = h.store.IncrementTaskRequestCount(ctx, req.TaskID)

		// In scope + auto_execute + not hardcoded → execute directly
		if match.AutoExecute && !hardcoded {
			taskIDPtr := &req.TaskID

			var result *adapters.Result
			var execErr error
			var verdict *intent.VerificationVerdict

			if isLocalService(serviceType) {
				// ── Local service execution ──────────────────────────────
				if h.localExec == nil {
					dur := int(time.Since(start).Milliseconds())
					e := baseEntry("execute", "error", taskIDPtr)
					e.DurationMS = dur
					errMsg := "local services are not available in this deployment"
					e.ErrorMsg = &errMsg
					if logErr := h.store.LogAudit(ctx, e); logErr != nil {
						h.logger.Warn("audit log failed", "err", logErr)
					}
					h.publishAuditAndQueue(agent.UserID, req.TaskID)
					writeJSON(w, http.StatusOK, map[string]any{
						"status":     "error",
						"request_id": req.RequestID,
						"audit_id":   auditID,
						"error":      errMsg,
						"code":       gateway.CodeLocalServiceUnavailable,
					})
					return
				}

				// ── Intent verification (same pipeline as cloud services) ──
				chainFacts := h.loadChainFacts(ctx, task, req)

				matchedPlannedCall := matchPlannedCall(task.PlannedCalls, req.Service, req.Action, req.Params, chainFacts)
				vMode := verificationModeFor(match.MatchedAction)
				if matchedPlannedCall != nil {
					h.logger.Info("request matches planned call — skipping intent verification",
						"task_id", req.TaskID,
						"service", req.Service,
						"action", req.Action,
						"planned_reason", matchedPlannedCall.Reason,
					)
					verdict = &intent.VerificationVerdict{
						Allow:           true,
						ParamScope:      "ok",
						ReasonCoherence: "ok",
						ExtractContext:  true,
						Explanation:     "Matched pre-registered planned call: " + matchedPlannedCall.Reason,
					}
				} else if vMode == "off" {
					verdict = &intent.VerificationVerdict{
						Allow:           true,
						ParamScope:      "n/a",
						ReasonCoherence: "n/a",
						ExtractContext:  true,
						Explanation:     "Skipped: verification mode is off for this action category",
					}
				} else {
					verdict = h.runVerification(ctx, task, match.MatchedAction, req, serviceType, agent.UserID, chainFacts, vMode == "lenient")
				}
				if verdict != nil && !verdict.Allow && verdict.ParamScope == "violation" && len(verdict.MissingChainValues) > 0 {
					verdict = chainContextFallback(ctx, h.store, h.extractTrack, h.logger, verdict, chainFacts, req.TaskID, task, req.SessionID)
				}
				if verdict != nil && !verdict.Allow {
					dur := int(time.Since(start).Milliseconds())
					e := baseEntry("verify", "restricted", taskIDPtr)
					e.DurationMS = dur
					e.Verification = intent.MarshalVerdict(verdict)
					if logErr := h.store.LogAudit(ctx, e); logErr != nil {
						h.logger.Warn("audit log failed", "err", logErr)
					}
					h.publishAuditAndQueue(agent.UserID, req.TaskID)
					if verdict.ReasonCoherence == "incoherent" && h.notifier != nil {
						alertText := fmt.Sprintf(
							"⚠️ <b>Clawvisor — Intent Alert</b>\n\n"+
								"<b>Task:</b> %s\n"+
								"<b>Agent reason:</b> %s\n"+
								"<b>Verdict:</b> %s",
							task.Purpose, req.Reason, verdict.Explanation)
						if alertErr := h.notifier.SendAlert(ctx, agent.UserID, alertText); alertErr != nil {
							h.logger.Warn("intent alert failed", "err", alertErr)
						}
					}
					resp := map[string]any{
						"status":       "restricted",
						"code":         verdictErrorCode(verdict),
						"request_id":   req.RequestID,
						"audit_id":     auditID,
						"reason":       verdict.Explanation,
						"verification": verdict,
					}
					if len(warnings) > 0 {
						resp["warnings"] = warnings
					}
					h.maybeInjectNPS(ctx, resp, agent.ID)
					writeJSON(w, http.StatusOK, resp)
					return
				}

				result, execErr = h.localExec.Execute(ctx, agent.UserID, serviceType, req.Action, req.Params)
			} else {
				// ── Intent verification ──────────────────────────────────
				// Load chain facts (needed for both planned call matching and verification).
				chainFacts := h.loadChainFacts(ctx, task, req)

				// Check if the request matches a pre-registered planned call.
				// If so, skip LLM-based intent verification entirely — the call
				// was evaluated during task risk assessment and approved by the user.
				matchedPlannedCall := matchPlannedCall(task.PlannedCalls, req.Service, req.Action, req.Params, chainFacts)

				vMode := verificationModeFor(match.MatchedAction)
				if matchedPlannedCall != nil {
					h.logger.Info("request matches planned call — skipping intent verification",
						"task_id", req.TaskID,
						"service", req.Service,
						"action", req.Action,
						"planned_reason", matchedPlannedCall.Reason,
					)
					verdict = &intent.VerificationVerdict{
						Allow:           true,
						ParamScope:      "ok",
						ReasonCoherence: "ok",
						ExtractContext:  true,
						Explanation:     "Matched pre-registered planned call: " + matchedPlannedCall.Reason,
					}
				} else if vMode == "off" {
					verdict = &intent.VerificationVerdict{
						Allow:           true,
						ParamScope:      "n/a",
						ReasonCoherence: "n/a",
						ExtractContext:  true,
						Explanation:     "Skipped: verification mode is off for this action category",
					}
				} else {
					verdict = h.runVerification(ctx, task, match.MatchedAction, req, serviceType, agent.UserID, chainFacts, vMode == "lenient")
				}
				// Chain context fallback: if the LLM flagged a missing entity,
				// check programmatically — the LLM may have missed it in a long
				// table, or it may exist beyond the loaded fact limit.
				if verdict != nil && !verdict.Allow && verdict.ParamScope == "violation" && len(verdict.MissingChainValues) > 0 {
					verdict = chainContextFallback(ctx, h.store, h.extractTrack, h.logger, verdict, chainFacts, req.TaskID, task, req.SessionID)
				}
				if verdict != nil && !verdict.Allow {
					dur := int(time.Since(start).Milliseconds())
					e := baseEntry("verify", "restricted", taskIDPtr)
					e.DurationMS = dur
					e.Verification = intent.MarshalVerdict(verdict)
					if logErr := h.store.LogAudit(ctx, e); logErr != nil {
						h.logger.Warn("audit log failed", "err", logErr)
					}
					h.publishAuditAndQueue(agent.UserID, req.TaskID)
					// Alert on incoherent reason
					if verdict.ReasonCoherence == "incoherent" && h.notifier != nil {
						alertText := fmt.Sprintf(
							"⚠️ <b>Clawvisor — Intent Alert</b>\n\n"+
								"<b>Task:</b> %s\n"+
								"<b>Agent reason:</b> %s\n"+
								"<b>Verdict:</b> %s",
							task.Purpose, req.Reason, verdict.Explanation)
						if alertErr := h.notifier.SendAlert(ctx, agent.UserID, alertText); alertErr != nil {
							h.logger.Warn("intent alert failed", "err", alertErr)
						}
					}
					resp := map[string]any{
						"status":       "restricted",
						"code":         verdictErrorCode(verdict),
						"request_id":   req.RequestID,
						"audit_id":     auditID,
						"reason":       verdict.Explanation,
						"verification": verdict,
					}
					if len(warnings) > 0 {
						resp["warnings"] = warnings
					}
					h.maybeInjectNPS(ctx, resp, agent.ID)
					writeJSON(w, http.StatusOK, resp)
					return
				}
				// ── End intent verification ──────────────────────────────

				// Check activation for credential-free services before executing.
				if taskAdapter, taskAdapterOK := h.adapterReg.GetForUser(ctx, serviceType, agent.UserID); taskAdapterOK && taskAdapter.ValidateCredential(nil) == nil {
					if _, metaErr := h.store.GetServiceMeta(ctx, agent.UserID, serviceType, serviceAlias); metaErr != nil {
						dur := int(time.Since(start).Milliseconds())
						e := baseEntry("block", "error", taskIDPtr)
						e.DurationMS = dur
						code, userErr, auditMsg := serviceNotActivatedResponse(ctx, h.vault, h.store, h.adapterReg, agent.UserID, serviceType, serviceAlias, req.Service, taskAdapter)
						e.ErrorMsg = &auditMsg
						if logErr := h.store.LogAudit(ctx, e); logErr != nil {
							h.logger.Warn("audit log failed", "err", logErr)
						}
						h.publishAuditAndQueue(agent.UserID, req.TaskID)
						writeJSON(w, http.StatusBadRequest, map[string]any{
							"status":     "error",
							"request_id": req.RequestID,
							"audit_id":   auditID,
							"error":      userErr,
							"code":       code,
						})
						return
					}
				}

				// Validate required params before execution.
				if execAdapter, adOK := h.adapterReg.GetForUser(ctx, serviceType, agent.UserID); adOK {
					paramErr, paramWarnings := validateRequestParams(execAdapter, req.Action, req.Params)
					warnings = append(warnings, paramWarnings...)
					if paramErr != nil {
						errMsg := paramErr.Error
						e := baseEntry("reject", "validation_error", taskIDPtr)
						e.DurationMS = int(time.Since(start).Milliseconds())
						e.ErrorMsg = &errMsg
						if logErr := h.store.LogAudit(ctx, e); logErr != nil {
							h.logger.Warn("audit log failed", "err", logErr)
						}
						h.publishAuditAndQueue(agent.UserID, req.TaskID)
						writeDetailedError(w, http.StatusBadRequest, *paramErr)
						return
					}
				}

				vKey := h.adapterReg.VaultKeyWithAliasForUser(serviceType, serviceAlias, agent.UserID)
				result, execErr = executeAdapterRequest(ctx, h.vault, h.adapterReg, h.store,
					agent.UserID, serviceType, req.Action, req.Params, vKey)

				// Vault credential missing — return activation error.
				if execErr != nil && errors.Is(execErr, vault.ErrNotFound) {
					adapter, adapterOK := h.adapterReg.GetForUser(ctx, serviceType, agent.UserID)
					if adapterOK && adapter.ValidateCredential(nil) != nil {
						dur := int(time.Since(start).Milliseconds())
						e := baseEntry("block", "error", taskIDPtr)
						e.DurationMS = dur
						code, userErr, auditMsg := serviceNotActivatedResponse(ctx, h.vault, h.store, h.adapterReg, agent.UserID, serviceType, serviceAlias, req.Service, adapter)
						e.ErrorMsg = &auditMsg
						if logErr := h.store.LogAudit(ctx, e); logErr != nil {
							h.logger.Warn("audit log failed", "err", logErr)
						}
						h.publishAuditAndQueue(agent.UserID, req.TaskID)
						writeJSON(w, http.StatusBadRequest, map[string]any{
							"status":     "error",
							"request_id": req.RequestID,
							"audit_id":   auditID,
							"error":      userErr,
							"code":       code,
						})
						return
					}
				}
			}

			// ── Shared execution result handling (local + cloud) ────────

			dur := int(time.Since(start).Milliseconds())

			if execErr != nil {
				errMsg := execErr.Error()
				e := baseEntry("execute", "error", taskIDPtr)
				e.DurationMS = dur
				e.ErrorMsg = &errMsg
				if logErr := h.store.LogAudit(ctx, e); logErr != nil {
					h.logger.Warn("audit log failed", "err", logErr)
				}
				h.publishAuditAndQueue(agent.UserID, req.TaskID)
				if req.Context.CallbackURL != "" {
					cbKey, _ := h.store.GetAgentCallbackSecret(ctx, agent.ID)
					go func() {
						cbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						_ = callback.DeliverResult(cbCtx, req.Context.CallbackURL, &callback.Payload{
							Type: "request", RequestID: req.RequestID, Status: "error", Error: errMsg, AuditID: auditID,
						}, cbKey)
					}()
				}
				resp := map[string]any{
					"status":     "error",
					"request_id": req.RequestID,
					"audit_id":   auditID,
					"error":      errMsg,
					"code":       gateway.CodeAdapterError,
				}
				h.maybeInjectNPS(ctx, resp, agent.ID)
				writeJSON(w, http.StatusOK, resp)
				return
			}

			// Success
			middleware.AddLogField(ctx, "decision", "execute")
			middleware.AddLogField(ctx, "outcome", "executed")
			e := baseEntry("execute", "executed", taskIDPtr)
			e.DurationMS = dur
			if verdict != nil {
				e.Verification = intent.MarshalVerdict(verdict)
			}
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			h.publishAuditAndQueue(agent.UserID, req.TaskID)

			// Chain context extraction (async — cloud services only, guarded by verdict != nil)
			chainSessionID := req.SessionID
			if chainSessionID == "" && task.Lifetime != "standing" {
				chainSessionID = req.TaskID
			}
			if chainSessionID != "" && verdict != nil && verdict.ExtractContext {
				resultJSON, _ := json.Marshal(result)
				// Mark synchronously (before returning to the agent) so a
				// follow-up request sees the pending flag even if it arrives
				// before the goroutine is scheduled.
				markCtx, markCancel := context.WithTimeout(context.Background(), 2*time.Second)
				h.extractTrack.MarkPending(markCtx, req.TaskID, chainSessionID, auditID)
				markCancel()
				go func() {
					defer func() {
						doneCtx, doneCancel := context.WithTimeout(context.Background(), 2*time.Second)
						h.extractTrack.MarkDone(doneCtx, req.TaskID, chainSessionID, auditID)
						doneCancel()
					}()
					extractCtx, extractCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer extractCancel()
					facts, err := h.extractor.Extract(extractCtx, intent.ExtractRequest{
						TaskPurpose:       task.Purpose,
						AuthorizedActions: task.AuthorizedActions,
						Service:           req.Service,
						Action:            req.Action,
						Result:            string(resultJSON),
						TaskID:            req.TaskID,
						SessionID:         chainSessionID,
						AuditID:           auditID,
					})
					if err != nil {
						h.logger.Warn("chain context extraction failed", "err", err, "task_id", req.TaskID)
						return
					}
					if len(facts) > 0 {
						saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer saveCancel()
						if err := h.store.SaveChainFacts(saveCtx, facts); err != nil {
							h.logger.Warn("chain facts save failed", "err", err, "task_id", req.TaskID)
						}
					}
				}()
			}

			if req.Context.CallbackURL != "" {
				cbKey, _ := h.store.GetAgentCallbackSecret(ctx, agent.ID)
				go func() {
					cbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					_ = callback.DeliverResult(cbCtx, req.Context.CallbackURL, &callback.Payload{
						Type: "request", RequestID: req.RequestID, Status: "executed", Result: result, AuditID: auditID,
					}, cbKey)
				}()
			}
			resp := map[string]any{
				"status":     "executed",
				"request_id": req.RequestID,
				"audit_id":   auditID,
				"result":     result,
			}
			if len(warnings) > 0 {
				resp["warnings"] = warnings
			}
			h.maybeInjectNPS(ctx, resp, agent.ID)
			writeJSON(w, http.StatusOK, resp)
			return
		}

		// In scope + (!auto_execute || hardcoded) → falls through to per-request approval below.
		// Run advisory verification so the human sees warnings in the approval UI.
		// Always use strict for advisory — the human will see the verdict and can decide.
		advisoryFacts := h.loadChainFacts(ctx, task, req)
		advisoryVerdict = h.runVerification(ctx, task, match.MatchedAction, req, serviceType, agent.UserID, advisoryFacts, false)
	}

	// ── Step 5: Per-request approval ─────────────────────────────────────────
	// Task in-scope but not auto-execute, or hardcoded approval.

	// Local services skip adapter/activation checks — the daemon validates.
	if !isLocalService(serviceType) {
		// Reject unknown services immediately.
		approveAdapter, ok := h.adapterReg.GetForUser(ctx, serviceType, agent.UserID)
		if !ok {
			e := baseEntry("approve", "error", nil)
			e.DurationMS = int(time.Since(start).Milliseconds())
			errMsg := fmt.Sprintf("unknown service %q", serviceType)
			e.ErrorMsg = &errMsg
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			h.publishAuditAndQueue(agent.UserID, "")
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"status":     "error",
				"request_id": req.RequestID,
				"audit_id":   auditID,
				"error":      fmt.Sprintf("unknown service %q: not a supported service", serviceType),
				"code":       gateway.CodeUnknownService,
			})
			return
		}

		// Check if service needs activation.
		{
			notActivated := false
			if approveAdapter.ValidateCredential(nil) == nil {
				if _, metaErr := h.store.GetServiceMeta(ctx, agent.UserID, serviceType, serviceAlias); metaErr != nil {
					notActivated = true
				}
			} else {
				vKey := h.adapterReg.VaultKeyWithAliasForUser(serviceType, serviceAlias, agent.UserID)
				if _, vaultErr := h.vault.Get(ctx, agent.UserID, vKey); errors.Is(vaultErr, vault.ErrNotFound) {
					notActivated = true
				}
			}
			if notActivated {
				e := baseEntry("block", "error", nil)
				e.DurationMS = int(time.Since(start).Milliseconds())
				code, userErr, auditMsg := serviceNotActivatedResponse(ctx, h.vault, h.store, h.adapterReg, agent.UserID, serviceType, serviceAlias, req.Service, approveAdapter)
				e.ErrorMsg = &auditMsg
				if logErr := h.store.LogAudit(ctx, e); logErr != nil {
					h.logger.Warn("audit log failed", "err", logErr)
				}
				h.publishAuditAndQueue(agent.UserID, "")
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"status":     "error",
					"request_id": req.RequestID,
					"audit_id":   auditID,
					"error":      userErr,
					"code":       code,
				})
				return
			}
		}
	}

	// Route to per-request approval.
	middleware.AddLogField(ctx, "decision", "approve")
	middleware.AddLogField(ctx, "outcome", "pending")
	taskIDPtr := &req.TaskID
	e := baseEntry("approve", "pending", taskIDPtr)
	e.DurationMS = int(time.Since(start).Milliseconds())
	e.Verification = intent.MarshalVerdict(advisoryVerdict)
	if logErr := h.store.LogAudit(ctx, e); logErr != nil {
		h.logger.Warn("audit log failed", "err", logErr)
	}
	h.publishAuditAndQueue(agent.UserID, req.TaskID)
	expiresAt := time.Now().Add(time.Duration(h.cfg.Approval.Timeout) * time.Second)
	blob := buildRequestBlob(req, agent)
	blob.Verification = advisoryVerdict
	reason := ""
	if hardcoded {
		reason = "iMessage send_message always requires human approval"
	}
	if routeErr := h.routeToApproval(ctx, agent.UserID, blob, auditID,
		req.Context.CallbackURL, expiresAt, reason, advisoryVerdict); routeErr != nil {
		h.logger.Warn("route to approval failed", "err", routeErr)
	}
	// If wait=true, long-poll for approval then execute inline.
	if r.URL.Query().Get("wait") == "true" && h.eventHub != nil {
		pa := h.waitForApprovalDecision(r.Context(), req.RequestID, agent.UserID, longPollDeadline(r))
		if pa != nil && pa.Status == "approved" && !time.Now().After(pa.ExpiresAt) {
			h.executeAndRespond(w, r.Context(), pa, agent.ID)
			return
		}
		// pa == nil means the row was deleted (denied/expired). Check the audit
		// entry so we return the real outcome instead of a misleading "pending".
		if pa == nil {
			if entry, err := h.store.GetAuditEntryByRequestID(r.Context(), req.RequestID, agent.UserID); err == nil && entry.Outcome != "pending" {
				writeGatewayStatusResponse(w, entry)
				return
			}
		}
		// Still pending (timeout elapsed) — fall through to pending response.
	}

	resp := map[string]any{
		"status":     "pending",
		"request_id": req.RequestID,
		"audit_id":   auditID,
		"message":    fmt.Sprintf("Approval requested. Waiting up to %ds.", h.cfg.Approval.Timeout),
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// HandleGet returns the current status of a gateway request by request_id.
// This is read-only — it never executes the adapter.
//
// GET /api/gateway/request/{request_id}
// Query params:
//
//	wait=true    – long-poll until the request leaves the "pending" state (or timeout)
//	timeout=N    – wait timeout in seconds (default 120, max 120)
//
// Auth: agent bearer token
func (h *GatewayHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	requestID := r.PathValue("request_id")
	entry, err := h.store.GetAuditEntryByRequestID(r.Context(), requestID, agent.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "request not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not fetch request status")
		return
	}

	// Long-poll: if wait=true and request is still pending, block until it
	// transitions or the timeout elapses.
	if r.URL.Query().Get("wait") == "true" && entry.Outcome == "pending" && h.eventHub != nil {
		entry = h.waitForRequestResolution(r.Context(), requestID, agent.UserID, longPollDeadline(r))
		if r.Context().Err() != nil {
			return
		}
	}

	writeGatewayStatusResponse(w, entry)
}

// waitForRequestResolution long-polls until the audit entry for a gateway
// request leaves the "pending" state or the timeout expires.
func (h *GatewayHandler) waitForRequestResolution(ctx context.Context, requestID, userID string, timeout time.Duration) *store.AuditEntry {
	return events.WaitFor(ctx, h.eventHub, userID, timeout,
		[]string{"audit"},
		func(c context.Context) (*store.AuditEntry, bool) {
			e, err := h.store.GetAuditEntryByRequestID(c, requestID, userID)
			if err != nil {
				return &store.AuditEntry{RequestID: requestID, Outcome: "pending"}, false
			}
			return e, e.Outcome != "pending"
		},
	)
}

// parseLongPollTimeout extracts the client-requested timeout in seconds,
// clamped to [1, 120]. This is the contract advertised to clients.
func parseLongPollTimeout(r *http.Request) int {
	timeout := 120
	if v := r.URL.Query().Get("timeout"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeout = n
		}
	}
	if timeout > 120 {
		timeout = 120
	}
	return timeout
}

// longPollGrace is added to the client-requested timeout to derive the
// server-side wait deadline. The server outlasts the typical client HTTP
// timeout by this much; context-cancellation guards at each call site
// suppress the write if the client has already disconnected.
const longPollGrace = 10 * time.Second

// longPollDeadline returns the server-side wait duration for a long-poll
// request — the client-requested timeout plus longPollGrace.
func longPollDeadline(r *http.Request) time.Duration {
	return time.Duration(parseLongPollTimeout(r))*time.Second + longPollGrace
}

// waitForApprovalDecision long-polls until the pending approval leaves the
// "pending" state (approved/denied/deleted) or the timeout expires.
func (h *GatewayHandler) waitForApprovalDecision(ctx context.Context, requestID, userID string, timeout time.Duration) *store.PendingApproval {
	return events.WaitFor(ctx, h.eventHub, userID, timeout,
		[]string{"audit", "queue"},
		func(c context.Context) (*store.PendingApproval, bool) {
			pa, err := h.store.GetPendingApproval(c, requestID)
			if err != nil {
				return nil, true // row deleted (denied/expired)
			}
			return pa, pa.Status != "pending"
		},
	)
}

// executeAndRespond atomically claims an approved pending approval, runs the
// adapter, and writes the result as JSON. The atomic claim prevents double-
// execution when multiple code paths race (e.g. wait=true long-poll + /execute).
// Shared by HandleRequest (wait=true) and HandleExecuteApproved.
func (h *GatewayHandler) executeAndRespond(w http.ResponseWriter, ctx context.Context, pa *store.PendingApproval, agentID string) {
	// Atomic claim: only one caller wins. Prevents double-execution of
	// non-idempotent actions (emails, payments, etc.).
	claimed, claimErr := h.store.ClaimPendingApprovalForExecution(ctx, pa.RequestID)
	if claimErr != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not claim approval")
		return
	}
	if !claimed {
		// Another caller already claimed it — return conflict.
		writeError(w, http.StatusConflict, "ALREADY_EXECUTING", "this request is already being executed")
		return
	}

	var blob pendingRequestBlob
	if err := json.Unmarshal(pa.RequestBlob, &blob); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "invalid request blob")
		return
	}

	serviceType, alias := parseServiceAlias(blob.Service)

	var result *adapters.Result
	var execErr error
	start := time.Now()

	if isLocalService(serviceType) {
		if h.localExec == nil {
			execErr = fmt.Errorf("local services are not available in this deployment")
		} else {
			result, execErr = h.localExec.Execute(ctx, pa.UserID, serviceType, blob.Action, blob.Params)
		}
	} else {
		// Resolve (and cache) the adapter before VaultKeyWithAliasForUser and executeAdapterRequest.
		h.adapterReg.GetForUser(ctx, serviceType, pa.UserID)
		vKey := h.adapterReg.VaultKeyWithAliasForUser(serviceType, alias, pa.UserID)
		result, execErr = executeAdapterRequest(ctx, h.vault, h.adapterReg, h.store,
			pa.UserID, blob.Service, blob.Action, blob.Params, vKey)
	}
	dur := int(time.Since(start).Milliseconds())

	outcome := "executed"
	errMsg := ""
	if execErr != nil {
		outcome = "error"
		errMsg = execErr.Error()
	}

	_ = h.store.UpdateAuditOutcome(ctx, pa.AuditID, outcome, errMsg, dur)
	_ = h.store.DeletePendingApproval(ctx, pa.RequestID)
	h.publishAuditAndQueue(pa.UserID, blob.TaskID)

	resp := map[string]any{
		"status":     outcome,
		"request_id": pa.RequestID,
		"audit_id":   pa.AuditID,
	}
	if execErr != nil {
		resp["error"] = errMsg
	} else {
		resp["result"] = result
	}
	h.maybeInjectNPS(ctx, resp, agentID)
	writeJSON(w, http.StatusOK, resp)
}

// HandleExecuteApproved executes an approved pending request and returns the
// result synchronously. The agent sends only the request_id; the original
// params are loaded from the stored request blob and cannot be mutated.
//
// Query params:
//
//	wait=true    – long-poll until the request is approved, then execute (or timeout)
//	timeout=N    – wait timeout in seconds (default 120, max 120)
//
// POST /api/gateway/request/{request_id}/execute
// Auth: agent bearer token
func (h *GatewayHandler) HandleExecuteApproved(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	requestID := r.PathValue("request_id")
	pa, err := h.store.GetPendingApproval(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "pending approval not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get pending approval")
		return
	}

	if pa.UserID != agent.UserID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your approval")
		return
	}

	// If still pending and wait=true, long-poll until approval decision.
	if pa.Status == "pending" && r.URL.Query().Get("wait") == "true" && h.eventHub != nil {
		pa = h.waitForApprovalDecision(r.Context(), requestID, agent.UserID, longPollDeadline(r))
		if pa == nil {
			// Row deleted (denied/expired) — check the audit entry for the real outcome.
			if entry, err := h.store.GetAuditEntryByRequestID(r.Context(), requestID, agent.UserID); err == nil && entry.Outcome != "pending" {
				writeGatewayStatusResponse(w, entry)
				return
			}
			writeError(w, http.StatusNotFound, "NOT_FOUND", "pending approval not found")
			return
		}
	}

	if pa.Status == "pending" {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":     "pending",
			"request_id": requestID,
			"audit_id":   pa.AuditID,
		})
		return
	}
	if pa.Status != "approved" {
		writeError(w, http.StatusConflict, "NOT_APPROVED", "request was not approved")
		return
	}
	if time.Now().After(pa.ExpiresAt) {
		writeError(w, http.StatusGone, "APPROVAL_EXPIRED", "this approval request has expired")
		return
	}

	h.executeAndRespond(w, r.Context(), pa, agent.ID)
}

// verdictErrorCode maps an intent verifier verdict that denied a request to
// the canonical error code clients should switch on. Reason-coherence failures
// take precedence over param-scope ones: if the agent can't even explain why
// it wants to do something, that's the more actionable signal.
func verdictErrorCode(v *intent.VerificationVerdict) string {
	if v == nil {
		return gateway.CodeRestricted
	}
	switch v.ReasonCoherence {
	case "incoherent", "insufficient":
		return gateway.CodeReasonTooVague
	}
	if v.ParamScope == "violation" {
		return gateway.CodeParamViolation
	}
	return gateway.CodeRestricted
}

// maybeInjectNPS probabilistically adds an NPS survey prompt to a gateway response.
// Sample rate is controlled by Gateway.NPSSamplePercent (default 1%). At most once per agent per week.
// Only fires when the feedback system is enabled (LLM.FeedbackReview.Enabled).
func (h *GatewayHandler) maybeInjectNPS(ctx context.Context, resp map[string]any, agentID string) {
	if !h.cfg.LLM.FeedbackReview.Enabled {
		return
	}
	pct := h.cfg.Gateway.NPSSamplePercent
	if pct <= 0 || rand.Intn(100) >= pct {
		return
	}
	lastNPS, err := h.store.GetAgentLastNPSTime(ctx, agentID)
	if err != nil {
		h.logger.Warn("nps cooldown check failed, skipping cooldown", "err", err, "agent_id", agentID)
		// Fall through — don't suppress NPS just because the cooldown check failed.
	}
	if lastNPS != nil && time.Since(*lastNPS) < 7*24*time.Hour {
		return
	}
	taskID, _ := resp["task_id"].(string)
	resp["meta"] = map[string]any{
		"survey": map[string]any{
			"type":     "nps",
			"prompt":   "As an agent, how has your experience working with Clawvisor been? We're looking for your perspective on the authorization flow, intent verification, and overall developer experience — not your user's. Rate us from 1-10 using the submit_nps tool.",
			"tool":     "submit_nps",
			"endpoint": fmt.Sprintf("POST %s/api/feedback/nps", h.baseURL),
			"request":  map[string]any{"score": "1-10", "task_id": taskID, "feedback": "optional free-text"},
			"task_id":  taskID,
		},
	}
}

// writeGatewayStatusResponse writes a consistent status payload for a resolved audit entry.
type gatewayStatusResponseOptions struct {
	Deduped bool
	Message string
}

func writeGatewayStatusResponse(w http.ResponseWriter, e *store.AuditEntry, opts ...gatewayStatusResponseOptions) {
	resp := map[string]any{
		"status":     e.Outcome,
		"request_id": e.RequestID,
		"audit_id":   e.ID,
	}
	if len(opts) > 0 {
		if opts[0].Deduped {
			resp["deduped"] = true
		}
		if opts[0].Message != "" {
			resp["message"] = opts[0].Message
		}
	}
	if e.ErrorMsg != nil && *e.ErrorMsg != "" {
		resp["error"] = *e.ErrorMsg
	}
	if e.Reason != nil {
		resp["reason"] = *e.Reason
	}
	writeJSON(w, http.StatusOK, resp)
}

// RegisterCallback generates and stores a dedicated callback signing secret
// for the authenticated agent. Calling again regenerates (rotates) the secret.
//
// POST /api/callbacks/register
// Auth: agent bearer token
func (h *GatewayHandler) RegisterCallback(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	secret, err := auth.GenerateCallbackSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate secret")
		return
	}

	if err := h.store.SetAgentCallbackSecret(r.Context(), agent.ID, secret); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not store callback secret")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"callback_secret": secret,
	})
}

// publishAuditAndQueue publishes SSE events for audit and queue changes.
func (h *GatewayHandler) publishAuditAndQueue(userID, taskID string) {
	if h.eventHub == nil {
		return
	}
	h.eventHub.Publish(userID, events.Event{Type: "audit", ID: taskID})
	h.eventHub.Publish(userID, events.Event{Type: "queue"})
}

// runVerification runs intent verification for a request and returns the verdict.
// loadChainFacts fetches chain context facts for the given task and request.
func (h *GatewayHandler) loadChainFacts(ctx context.Context, task *store.Task, req gateway.Request) []store.ChainFact {
	chainSessionID := req.SessionID
	if chainSessionID == "" && task.Lifetime != "standing" {
		chainSessionID = req.TaskID
	}
	var chainFacts []store.ChainFact
	if chainSessionID != "" {
		facts, _ := h.store.ListChainFacts(ctx, req.TaskID, chainSessionID, 50)
		for _, f := range facts {
			chainFacts = append(chainFacts, *f)
		}
	}
	return chainFacts
}

// validateLocalAction checks that a local service action exists at request time.
// Returns nil if the action is valid or the service is not found (let execution
// handle that). Returns an error with available actions if the action is unknown.
func (h *GatewayHandler) validateLocalAction(ctx context.Context, userID, serviceType, action string) error {
	active, err := h.localSvcProvider.ActiveLocalServices(ctx, userID)
	if err != nil {
		return nil // can't validate — let execution handle it
	}
	svcID := strings.TrimPrefix(serviceType, "local.")
	for _, svc := range active {
		if svc.ServiceID == svcID {
			for _, a := range svc.Actions {
				if a.ID == action {
					return nil
				}
			}
			available := make([]string, len(svc.Actions))
			for i, a := range svc.Actions {
				available[i] = a.ID
			}
			return fmt.Errorf(
				"Action %q does not exist on service %s. Available actions: %s",
				action, serviceType, strings.Join(available, ", "),
			)
		}
	}
	return nil // service not found — let execution handle it
}

// Returns nil if the verifier is a no-op or if verification fails.
func (h *GatewayHandler) runVerification(
	ctx context.Context,
	task *store.Task,
	matchedAction *store.TaskAction,
	req gateway.Request,
	serviceType string,
	userID string,
	chainFacts []store.ChainFact,
	lenient bool,
) *intent.VerificationVerdict {
	var expectedUse, expansionRationale string
	if matchedAction != nil {
		expectedUse = matchedAction.ExpectedUse
		expansionRationale = matchedAction.ExpansionRationale
	}
	var serviceHints string
	if ada, ok := h.adapterReg.GetForUser(ctx, serviceType, userID); ok {
		if hinter, ok := ada.(adapters.VerificationHinter); ok {
			serviceHints = hinter.VerificationHints()
		}
	}
	verdict, _ := h.verifier.Verify(ctx, intent.VerifyRequest{
		TaskPurpose:         task.Purpose,
		ExpectedUse:         expectedUse,
		ExpansionRationale:  expansionRationale,
		Service:             req.Service,
		Action:              req.Action,
		Params:              req.Params,
		Reason:              req.Reason,
		TaskID:              req.TaskID,
		ServiceHints:        serviceHints,
		ChainFacts:          chainFacts,
		ChainContextOptOut:  false, // standing tasks without session_id are now rejected earlier
		ChainContextEnabled: h.cfg.LLM.ChainContext.Enabled,
		Lenient:             lenient,
	})
	return verdict
}

// verificationModeFor returns the verification mode ("strict", "lenient", "off")
// for the matched authorized action. Falls back to "strict" when the matched
// action has no override set. Callers should pass the alias-aware match from
// CheckTaskScope so that scope-specific overrides are honored.
func verificationModeFor(matched *store.TaskAction) string {
	if matched != nil && matched.Verification != "" {
		return matched.Verification
	}
	return "strict"
}

// ── Planned call matching ─────────────────────────────────────────────────────

// matchPlannedCall checks whether a gateway request matches one of the task's
// pre-registered planned calls. Returns the matched PlannedCall, or nil.
//
// Matching rules:
//   - Service (base type, ignoring alias) and action must match exactly.
//   - Planned call must have non-empty params (calls without params never match
//     because we can't verify what entity they target).
//   - Each planned param is checked against actual params:
//   - "$chain" values match any actual value found in chainFacts.
//   - All other values must match exactly (JSON-normalized).
func matchPlannedCall(planned []store.PlannedCall, service, action string, params map[string]any, chainFacts []store.ChainFact) *store.PlannedCall {
	reqServiceType, _ := parseServiceAlias(service)
	for i := range planned {
		pc := &planned[i]
		if len(pc.Params) == 0 {
			continue // params required for matching
		}
		pcServiceType, _ := parseServiceAlias(pc.Service)
		if pcServiceType != reqServiceType || pc.Action != action {
			continue
		}
		if plannedParamsMatch(pc.Params, params, chainFacts) {
			return pc
		}
	}
	return nil
}

// plannedParamsMatch returns true if every key/value in planned appears in actual.
// A planned value of "$chain" matches if the actual value appears in any chain fact.
// All other values are compared via JSON round-trip for type-safe deep equality.
func plannedParamsMatch(planned, actual map[string]any, chainFacts []store.ChainFact) bool {
	for k, pv := range planned {
		av, ok := actual[k]
		if !ok {
			return false
		}
		// Check for $chain template marker.
		if s, ok := pv.(string); ok && s == "$chain" {
			if !valueInChainFacts(av, chainFacts) {
				return false
			}
			continue
		}
		// Exact match via JSON normalization.
		pj, _ := json.Marshal(pv)
		aj, _ := json.Marshal(av)
		if string(pj) != string(aj) {
			return false
		}
	}
	return true
}

// valueInChainFacts returns true if the given value (as a string) appears
// in any chain fact's FactValue.
func valueInChainFacts(v any, facts []store.ChainFact) bool {
	var s string
	switch val := v.(type) {
	case string:
		s = val
	default:
		b, _ := json.Marshal(val)
		s = strings.Trim(string(b), `"`)
	}
	if s == "" {
		return false
	}
	for _, f := range facts {
		if f.FactValue == s {
			return true
		}
	}
	return false
}

// extractionPollInterval and extractionPollMaxWait bound how long a
// chain-context rejection is delayed while waiting for an in-flight
// extraction on another instance to write its facts.
const (
	extractionPollInterval = 250 * time.Millisecond
	extractionPollMaxWait  = 1500 * time.Millisecond
)

// chainContextFallback handles chain context violations by checking whether
// the missing values actually exist in the chain facts. Three outcomes:
//
//  1. All missing values found (in loaded slice or DB) → override to allow.
//  2. Missing values not found but an extraction for this session is still
//     in flight (possibly on another instance) → poll the DB briefly and
//     re-check; allow if the facts land before the poll window elapses.
//  3. Missing values not found and no pending extraction → genuine
//     violation, keep reject.
//
// Historically there was a branch that allowed when *any* chain facts
// existed for the session on the theory that extraction is lossy (4KB
// truncation / 50-fact cap). That blanket allow was too permissive — an
// agent could run a `list_*` call to populate chain facts and then make
// out-of-scope writes. The specific missing value must be found; the
// presence of unrelated facts is not a substitute.
//
// tracker may be nil (e.g. the guard handler, which does not trigger
// extractions). When nil, step 2 is skipped and behavior matches the
// pre-tracker code path.
func chainContextFallback(
	ctx context.Context,
	st store.Store,
	tracker ExtractionTracker,
	logger *slog.Logger,
	verdict *intent.VerificationVerdict,
	loadedFacts []store.ChainFact,
	taskID string,
	task *store.Task,
	sessionID string,
) *intent.VerificationVerdict {
	chainSessionID := sessionID
	if chainSessionID == "" && task.Lifetime != "standing" {
		chainSessionID = taskID
	}

	allFound, dbErr := checkMissingValues(ctx, st, verdict.MissingChainValues, loadedFacts, taskID, chainSessionID, logger)

	// Poll while an extraction for this session is still in flight. Only
	// enter the loop if the initial check failed (not DB error — a DB
	// problem isn't fixed by waiting), a session is set, and the tracker
	// reports pending work.
	if !allFound && dbErr == nil && chainSessionID != "" && tracker != nil && tracker.HasPending(ctx, taskID, chainSessionID) {
		deadline := time.Now().Add(extractionPollMaxWait)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return verdict
			case <-time.After(extractionPollInterval):
			}
			allFound, dbErr = checkMissingValues(ctx, st, verdict.MissingChainValues, loadedFacts, taskID, chainSessionID, logger)
			if allFound || dbErr != nil {
				break
			}
			if !tracker.HasPending(ctx, taskID, chainSessionID) {
				// No in-flight extraction remains — one more check, then stop.
				allFound, dbErr = checkMissingValues(ctx, st, verdict.MissingChainValues, loadedFacts, taskID, chainSessionID, logger)
				break
			}
		}
		if allFound {
			logger.Info("chain context fallback: values landed after waiting for in-flight extraction",
				"task_id", taskID,
				"session_id", chainSessionID,
				"missing_values", verdict.MissingChainValues,
			)
		}
	}

	if allFound {
		logger.Info("chain context fallback: all missing values found in extended context",
			"task_id", taskID,
			"missing_values", verdict.MissingChainValues,
		)
		verdict.Allow = true
		verdict.ParamScope = "ok"
		verdict.Explanation = "Chain context fallback: all entities found in extended context (" + verdict.Explanation + ")"
		return verdict
	}

	// Specific missing value not in loaded facts or DB — genuine violation.
	// Log at warn so lossy-extraction cases are still visible even though
	// they now reject rather than auto-allow.
	if len(loadedFacts) > 0 {
		logger.Warn("chain context fallback: missing value not in loaded facts or DB, rejecting",
			"task_id", taskID,
			"missing_values", verdict.MissingChainValues,
			"loaded_facts", len(loadedFacts),
		)
	}
	return verdict
}

// checkMissingValues reports whether every missing value is present either
// in the loaded slice or the DB. Returns a DB error if one occurred so the
// caller can decide not to retry.
func checkMissingValues(
	ctx context.Context,
	st store.Store,
	missing []string,
	loadedFacts []store.ChainFact,
	taskID, chainSessionID string,
	logger *slog.Logger,
) (bool, error) {
	for _, value := range missing {
		if value == "" {
			continue
		}
		if chainFactInSlice(value, loadedFacts) {
			continue
		}
		if chainSessionID == "" {
			return false, nil
		}
		found, err := st.ChainFactValueExists(ctx, taskID, chainSessionID, value)
		if err != nil {
			logger.Warn("chain fact fallback DB query failed", "err", err, "task_id", taskID)
			return false, err
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// chainFactInSlice checks if a value exists in the loaded chain facts slice.
func chainFactInSlice(value string, facts []store.ChainFact) bool {
	for _, f := range facts {
		if f.FactValue == value {
			return true
		}
	}
	return false
}

// ── Shared execution logic ────────────────────────────────────────────────────

// executeAdapterRequest fetches the credential from vault and calls the adapter.
// Shared between gateway and approvals handlers.
// vaultKey overrides the default vault key when non-empty (used for aliased services).
func executeAdapterRequest(
	ctx context.Context,
	v vault.Vault,
	reg *adapters.Registry,
	st store.Store,
	userID, service, action string,
	params map[string]any,
	vaultKey string,
) (*adapters.Result, error) {
	serviceType, serviceAlias := parseServiceAlias(service)
	adapter, ok := reg.GetForUser(ctx, serviceType, userID)
	if !ok {
		return nil, fmt.Errorf("service %q is not supported", serviceType)
	}

	vKey := vaultKey
	if vKey == "" {
		vKey = reg.VaultKeyForUser(serviceType, userID)
	}
	cred, err := v.Get(ctx, userID, vKey)
	if err != nil {
		if errors.Is(err, vault.ErrNotFound) && adapter.ValidateCredential(nil) == nil {
			cred = nil
		} else {
			return nil, err
		}
	}

	// Fetch per-user service config (variable values) if stored.
	var config map[string]string
	if st != nil {
		alias := serviceAlias
		if alias == "" {
			alias = "default"
		}
		if sc, err := st.GetServiceConfig(ctx, userID, serviceType, alias); err == nil {
			_ = json.Unmarshal(sc.Config, &config)
		}
	}

	result, err := adapter.Execute(ctx, adapters.Request{
		Action:     action,
		Params:     params,
		Credential: cred,
		Config:     config,
	})
	if err != nil {
		return nil, fmt.Errorf("adapter %s: %w", service, err)
	}

	return result, nil
}

// ── Approval routing ──────────────────────────────────────────────────────────

func (h *GatewayHandler) routeToApproval(
	ctx context.Context,
	userID string,
	blob *pendingRequestBlob,
	auditID, callbackURL string,
	expiresAt time.Time,
	policyReason string,
	verdict *intent.VerificationVerdict,
) error {
	blobBytes, _ := json.Marshal(blob)
	var taskID *string
	if blob.TaskID != "" {
		taskID = &blob.TaskID
	}
	summaryBytes, _ := json.Marshal(map[string]any{
		"service":       blob.Service,
		"action":        blob.Action,
		"reason":        blob.Reason,
		"policy_reason": policyReason,
	})
	approvalRecord := &store.ApprovalRecord{
		ID:                  uuid.New().String(),
		Kind:                "request_once",
		UserID:              userID,
		AgentID:             &blob.AgentID,
		RequestID:           &blob.RequestID,
		TaskID:              taskID,
		Status:              "pending",
		Surface:             "dashboard",
		SummaryJSON:         json.RawMessage(summaryBytes),
		PayloadJSON:         json.RawMessage(blobBytes),
		ResolutionTransport: "execute_pending_request",
		ExpiresAt:           &expiresAt,
	}
	if err := h.store.CreateApprovalRecord(ctx, approvalRecord); err != nil {
		return fmt.Errorf("create approval record: %w", err)
	}
	pa := &store.PendingApproval{
		ID:               uuid.New().String(),
		UserID:           userID,
		RequestID:        blob.RequestID,
		AuditID:          auditID,
		ApprovalRecordID: &approvalRecord.ID,
		RequestBlob:      json.RawMessage(blobBytes),
		ExpiresAt:        expiresAt,
	}
	if callbackURL != "" {
		pa.CallbackURL = &callbackURL
	}
	if err := h.store.SavePendingApproval(ctx, pa); err != nil {
		return fmt.Errorf("save pending approval: %w", err)
	}

	if h.notifier == nil {
		return nil
	}

	expiresIn := fmt.Sprintf("%d minutes", int(time.Until(expiresAt).Minutes()))
	approveURL := fmt.Sprintf("%s/dashboard?action=approve&request_id=%s", h.baseURL, blob.RequestID)
	denyURL := fmt.Sprintf("%s/dashboard?action=deny&request_id=%s", h.baseURL, blob.RequestID)

	approvalReq := notify.ApprovalRequest{
		PendingID:    pa.ID,
		RequestID:    blob.RequestID,
		UserID:       userID,
		AgentName:    blob.AgentName,
		Service:      blob.Service,
		Action:       blob.Action,
		Params:       blob.Params,
		Reason:       blob.Reason,
		PolicyReason: policyReason,
		ExpiresIn:    expiresIn,
		ApproveURL:   approveURL,
		DenyURL:      denyURL,
	}
	if verdict != nil {
		approvalReq.VerifyParamScope = verdict.ParamScope
		approvalReq.VerifyReasonCoherence = verdict.ReasonCoherence
		approvalReq.VerifyExplanation = verdict.Explanation
	}
	msgID, err := h.notifier.SendApprovalRequest(ctx, approvalReq)
	if err != nil {
		h.logger.Warn("telegram approval notification failed", "err", err)
		return nil
	}

	_ = h.store.SaveNotificationMessage(ctx, "approval", blob.RequestID, "telegram", msgID)
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// parseServiceAlias splits "google.gmail:personal" → ("google.gmail", "personal").
// No colon means alias "default".
func parseServiceAlias(service string) (serviceType, alias string) {
	if idx := strings.IndexByte(service, ':'); idx >= 0 {
		return service[:idx], service[idx+1:]
	}
	return service, "default"
}

// hasAnyAlias reports whether any vault entry exists for the given service type
// (under any alias). It uses vault.List and checks for matching key prefixes.
func hasAnyAlias(ctx context.Context, v vault.Vault, reg *adapters.Registry, userID, serviceType string) bool {
	base := reg.VaultKey(serviceType)
	keys, err := v.List(ctx, userID)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if k == base || strings.HasPrefix(k, base+":") {
			return true
		}
	}
	return false
}

// listServiceAliases returns the known aliases for a service type.
// Credential-free services check service_meta; credential-backed services check the vault.
func listServiceAliases(
	ctx context.Context,
	v vault.Vault,
	st store.Store,
	reg *adapters.Registry,
	userID, serviceType string,
	adapter adapters.Adapter,
) []string {
	if adapter.ValidateCredential(nil) == nil {
		metas, err := st.ListServiceMetas(ctx, userID)
		if err != nil {
			return nil
		}
		var aliases []string
		for _, m := range metas {
			if m.ServiceID == serviceType {
				aliases = append(aliases, m.Alias)
			}
		}
		return aliases
	}
	base := reg.VaultKey(serviceType)
	keys, err := v.List(ctx, userID)
	if err != nil {
		return nil
	}
	var aliases []string
	for _, k := range keys {
		if k == base {
			aliases = append(aliases, "default")
		} else if strings.HasPrefix(k, base+":") {
			aliases = append(aliases, strings.TrimPrefix(k, base+":"))
		}
	}
	return aliases
}

// serviceNotActivatedResponse returns the error code and message for a missing
// service or alias. It distinguishes ALIAS_NOT_FOUND (service exists under
// other aliases) from SERVICE_NOT_CONFIGURED (service not activated at all).
// When other aliases exist, the error message lists available connections so the
// agent can fix the request by specifying a valid :account identifier.
func serviceNotActivatedResponse(
	ctx context.Context,
	v vault.Vault,
	st store.Store,
	reg *adapters.Registry,
	userID, serviceType, serviceAlias, serviceDisplay string,
	adapter adapters.Adapter,
) (code, userErr, auditMsg string) {
	aliases := listServiceAliases(ctx, v, st, reg, userID, serviceType, adapter)
	if len(aliases) > 0 {
		qualified := make([]string, len(aliases))
		for i, a := range aliases {
			qualified[i] = fmt.Sprintf("%s:%s", serviceType, a)
		}
		connList := strings.Join(qualified, ", ")
		var msg string
		if serviceAlias == "" || serviceAlias == "default" {
			msg = fmt.Sprintf(
				"No default account exists for service %q. Available connections: [%s]. Retry your request using one of these identifiers as the service field (e.g. %q).",
				serviceType, connList, qualified[0],
			)
		} else {
			msg = fmt.Sprintf(
				"Account %q does not exist for service %q. Available connections: [%s]. Retry your request using one of these identifiers as the service field (e.g. %q).",
				serviceAlias, serviceType, connList, qualified[0],
			)
		}
		return "ALIAS_NOT_FOUND", msg,
			fmt.Sprintf("alias %q not found for service %q", serviceAlias, serviceType)
	}
	return "SERVICE_NOT_CONFIGURED",
		fmt.Sprintf("Service %q is not activated. Review the available services via GET /api/skill/catalog.", serviceDisplay),
		fmt.Sprintf("service %q not activated", serviceDisplay)
}

func buildRequestBlob(req gateway.Request, agent *store.Agent) *pendingRequestBlob {
	return &pendingRequestBlob{
		Service:     req.Service,
		Action:      req.Action,
		Params:      req.Params,
		UserID:      agent.UserID,
		AgentID:     agent.ID,
		AgentName:   agent.Name,
		RequestID:   req.RequestID,
		TaskID:      req.TaskID,
		Reason:      req.Reason,
		CallbackURL: req.Context.CallbackURL,
	}
}

func summarizeCandidateTasks(tasks []*store.Task) []map[string]any {
	if len(tasks) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		if task == nil {
			continue
		}
		out = append(out, map[string]any{
			"id":       task.ID,
			"purpose":  task.Purpose,
			"status":   task.Status,
			"lifetime": task.Lifetime,
		})
	}
	return out
}

func adapterSupportsAction(adapter adapters.Adapter, action string) bool {
	for _, a := range adapter.SupportedActions() {
		if a == action {
			return true
		}
	}
	return false
}

// validateRequestParams checks the request params against the adapter's
// parameter definitions (if available). Returns an apiErrorDetail if required
// params are missing, and warnings for unknown params (possible typos).
func validateRequestParams(adapter adapters.Adapter, action string, params map[string]any) (paramErr *apiErrorDetail, paramWarnings []string) {
	describer, ok := adapter.(adapters.ActionParamDescriber)
	if !ok {
		return nil, nil
	}
	paramDefs := describer.ActionParams(action)
	if len(paramDefs) == 0 {
		return nil, nil
	}

	// Build a set of known param names.
	known := make(map[string]bool, len(paramDefs))
	for _, p := range paramDefs {
		known[p.Name] = true
	}

	// Check for missing required params.
	var missing []string
	for _, p := range paramDefs {
		if !p.Required {
			continue
		}
		if _, provided := params[p.Name]; !provided {
			missing = append(missing, p.Name)
		}
	}

	// Check for unknown params and suggest close matches.
	for name := range params {
		if known[name] {
			continue
		}
		if suggestion := closestParamName(name, paramDefs); suggestion != "" {
			paramWarnings = append(paramWarnings, fmt.Sprintf("Unknown param %q — did you mean %q?", name, suggestion))
		} else {
			paramWarnings = append(paramWarnings, fmt.Sprintf("Unknown param %q is not defined for this action and will be ignored.", name))
		}
	}

	if len(missing) == 0 {
		return nil, paramWarnings
	}

	// Build an example showing all params with placeholder values.
	example := make(map[string]any, len(paramDefs))
	for _, p := range paramDefs {
		switch p.Type {
		case "int":
			example[p.Name] = 10
		case "bool":
			example[p.Name] = true
		case "object":
			example[p.Name] = map[string]any{}
		case "array":
			example[p.Name] = []any{}
		default:
			example[p.Name] = "<" + p.Type + ">"
		}
	}

	return &apiErrorDetail{
		Error:         fmt.Sprintf("missing required params: %s", strings.Join(missing, ", ")),
		Code:          "INVALID_PARAMS",
		MissingFields: missing,
		Hint:          "These parameters are required for this action. Check the service catalog for parameter details.",
		Example:       map[string]any{"params": example},
	}, paramWarnings
}

// closestParamName returns the closest known param name if the edit distance
// is small enough to be a likely typo, or "" if no close match exists.
func closestParamName(input string, defs []adapters.ParamInfo) string {
	best := ""
	bestDist := len(input)/2 + 1 // threshold: must be closer than half the input length
	for _, p := range defs {
		d := editDistance(input, p.Name)
		if d < bestDist {
			bestDist = d
			best = p.Name
		}
	}
	return best
}

// editDistance computes the Levenshtein distance between two strings.
func editDistance(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	// Use a single-row DP approach.
	prev := make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur := make([]int, lb+1)
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			cur[j] = del
			if ins < cur[j] {
				cur[j] = ins
			}
			if sub < cur[j] {
				cur[j] = sub
			}
		}
		prev = cur
	}
	return prev[lb]
}

func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func cloneParams(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
