package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	Service     string         `json:"service"`
	Action      string         `json:"action"`
	Params      map[string]any `json:"params"`
	UserID      string         `json:"user_id"`
	AgentID     string         `json:"agent_id"`
	AgentName   string         `json:"agent_name"`
	RequestID   string         `json:"request_id"`
	TaskID      string         `json:"task_id"`
	Reason      string         `json:"reason"`
	CallbackURL  string                    `json:"callback_url"`
	Verification *intent.VerificationVerdict `json:"verification,omitempty"`
}

// GatewayHandler handles POST /api/gateway/request.
type GatewayHandler struct {
	store        store.Store
	vault        vault.Vault
	adapterReg   *adapters.Registry
	notifier     notify.Notifier // may be nil if Telegram not configured
	verifier     intent.Verifier
	extractor    intent.Extractor
	cfg          config.Config
	logger       *slog.Logger
	baseURL  string
	eventHub *events.Hub
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
	eventHub *events.Hub,
) *GatewayHandler {
	return &GatewayHandler{
		store: st, vault: v, adapterReg: adapterReg,
		notifier: notifier, verifier: verifier, extractor: extractor,
		cfg: cfg, logger: logger, baseURL: baseURL,
		eventHub: eventHub,
	}
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
	if req.Service == "" || req.Action == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service and action are required")
		return
	}

	middleware.AddLogField(ctx, "service", req.Service)
	middleware.AddLogField(ctx, "action", req.Action)

	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "MISSING_REASON", "reason is required on every gateway request")
		return
	}
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
		// Dedup: if this request_id was already processed, return the existing outcome.
		if existing, err := h.store.GetAuditEntryByRequestID(ctx, req.RequestID, agent.UserID); err == nil {
			writeGatewayStatusResponse(w, existing)
			return
		}
	}
	middleware.AddLogField(ctx, "request_id", req.RequestID)

	paramsSafe, _ := json.Marshal(format.StripSecrets(cloneParams(req.Params)))

	auditID := uuid.New().String()

	// baseEntry builds an AuditEntry with fields common to all outcomes.
	baseEntry := func(decision, outcome string, taskID *string) *store.AuditEntry {
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

	// ── Step 1: Check restrictions ────────────────────────────────────────────
	// Check both the full service (with alias) and the base service type.
	restriction, _ := h.store.MatchRestriction(ctx, agent.UserID, req.Service, req.Action)
	if restriction == nil && serviceAlias != "default" {
		restriction, _ = h.store.MatchRestriction(ctx, agent.UserID, serviceType, req.Action)
	}
	if restriction != nil {
		middleware.AddLogField(ctx, "decision", "block")
		middleware.AddLogField(ctx, "outcome", "blocked")
		e := baseEntry("block", "blocked", nil)
		e.DurationMS = int(time.Since(start).Milliseconds())
		if logErr := h.store.LogAudit(ctx, e); logErr != nil {
			h.logger.Warn("audit log failed", "err", logErr)
		}
		h.publishAuditAndQueue(agent.UserID, "")
		reason := restriction.Reason
		if reason == "" {
			reason = fmt.Sprintf("Restricted: %s:%s is blocked", restriction.Service, restriction.Action)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "blocked",
			"request_id": req.RequestID,
			"audit_id":   auditID,
			"reason":     reason,
		})
		return
	}

	// ── Step 2: Task ID required ─────────────────────────────────────────────
	if req.TaskID == "" {
		writeError(w, http.StatusBadRequest, "TASK_REQUIRED",
			"task_id is required; create a task first via POST /api/tasks")
		return
	}

	// ── Step 3: Hardcoded approval check ─────────────────────────────────────
	hardcoded := RequiresHardcodedApproval(serviceType, req.Action)

	// ── Step 4: Task scope enforcement ───────────────────────────────────────
	var advisoryVerdict *intent.VerificationVerdict
	{
		task, taskErr := h.store.GetTask(ctx, req.TaskID)
		if taskErr != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "task not found")
			return
		}
		if task.UserID != agent.UserID {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "task does not belong to this agent's user")
			return
		}
		if task.ExpiresAt != nil && time.Now().After(*task.ExpiresAt) {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":  "task_expired",
				"task_id": req.TaskID,
				"message": "Task has expired. Use POST /api/tasks/{id}/expand to extend.",
			})
			return
		}
		if task.Status != "active" {
			writeError(w, http.StatusConflict, "INVALID_STATE",
				fmt.Sprintf("task is %s, not active", task.Status))
			return
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
			writeJSON(w, http.StatusOK, map[string]any{
				"status":     "pending_scope_expansion",
				"task_id":    req.TaskID,
				"request_id": req.RequestID,
				"audit_id":   auditID,
				"message":    msg,
			})
			return
		}

		_ = h.store.IncrementTaskRequestCount(ctx, req.TaskID)

		// In scope + auto_execute + not hardcoded → execute directly
		if match.AutoExecute && !hardcoded {
			taskIDPtr := &req.TaskID

			// ── Intent verification ──────────────────────────────────────
			verdict := h.runVerification(ctx, task, match.MatchedAction, req, serviceType)
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
				writeJSON(w, http.StatusOK, map[string]any{
					"status":       "restricted",
					"request_id":   req.RequestID,
					"audit_id":     auditID,
					"reason":       verdict.Explanation,
					"verification": verdict,
				})
				return
			}
			// ── End intent verification ──────────────────────────────────

			// Check activation for credential-free services before executing.
			if taskAdapter, taskAdapterOK := h.adapterReg.Get(serviceType); taskAdapterOK && taskAdapter.ValidateCredential(nil) == nil {
				if _, metaErr := h.store.GetServiceMeta(ctx, agent.UserID, serviceType, serviceAlias); metaErr != nil {
					dur := int(time.Since(start).Milliseconds())
					e := baseEntry("block", "error", taskIDPtr)
					e.DurationMS = dur
					code, userErr, auditMsg := serviceNotActivatedResponse(ctx, h.vault, h.store, agent.UserID, serviceType, serviceAlias, req.Service, taskAdapter)
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

			vKey := vaultKeyForServiceAlias(serviceType, serviceAlias)
			result, execErr := executeAdapterRequest(ctx, h.vault, h.adapterReg,
				agent.UserID, serviceType, req.Action, req.Params, vKey)
			dur := int(time.Since(start).Milliseconds())

			if execErr != nil {
				if errors.Is(execErr, vault.ErrNotFound) {
					// Vault credential missing — fail immediately.
					adapter, adapterOK := h.adapterReg.Get(serviceType)
					if adapterOK && adapter.ValidateCredential(nil) != nil {
						e := baseEntry("block", "error", taskIDPtr)
						e.DurationMS = dur
						code, userErr, auditMsg := serviceNotActivatedResponse(ctx, h.vault, h.store, agent.UserID, serviceType, serviceAlias, req.Service, adapter)
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
						_ = callback.DeliverResult(context.Background(), req.Context.CallbackURL, &callback.Payload{
							Type: "request", RequestID: req.RequestID, Status: "error", Error: errMsg, AuditID: auditID,
						}, cbKey)
					}()
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"status":     "error",
					"request_id": req.RequestID,
					"audit_id":   auditID,
					"error":      errMsg,
					"code":       "EXECUTION_ERROR",
				})
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

			// Chain context extraction (async — after response is written)
			chainSessionID := req.SessionID
			if chainSessionID == "" && task.Lifetime != "standing" {
				chainSessionID = req.TaskID
			}
			if chainSessionID != "" && verdict != nil && verdict.ExtractContext {
				resultJSON, _ := json.Marshal(result)
				go func() {
					facts, err := h.extractor.Extract(context.Background(), intent.ExtractRequest{
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
						if err := h.store.SaveChainFacts(context.Background(), facts); err != nil {
							h.logger.Warn("chain facts save failed", "err", err, "task_id", req.TaskID)
						}
					}
				}()
			}

			if req.Context.CallbackURL != "" {
				cbKey, _ := h.store.GetAgentCallbackSecret(ctx, agent.ID)
				go func() {
					_ = callback.DeliverResult(context.Background(), req.Context.CallbackURL, &callback.Payload{
						Type: "request", RequestID: req.RequestID, Status: "executed", Result: result, AuditID: auditID,
					}, cbKey)
				}()
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"status":     "executed",
				"request_id": req.RequestID,
				"audit_id":   auditID,
				"result":     result,
			})
			return
		}

		// In scope + (!auto_execute || hardcoded) → falls through to per-request approval below.
		// Run advisory verification so the human sees warnings in the approval UI.
		advisoryVerdict = h.runVerification(ctx, task, match.MatchedAction, req, serviceType)
	}

	// ── Step 5: Per-request approval ─────────────────────────────────────────
	// Task in-scope but not auto-execute, or hardcoded approval.

	// Reject unknown services immediately.
	approveAdapter, ok := h.adapterReg.Get(serviceType)
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
			"code":       "UNKNOWN_SERVICE",
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
			vKey := vaultKeyForServiceAlias(serviceType, serviceAlias)
			if _, vaultErr := h.vault.Get(ctx, agent.UserID, vKey); errors.Is(vaultErr, vault.ErrNotFound) {
				notActivated = true
			}
		}
		if notActivated {
			e := baseEntry("block", "error", nil)
			e.DurationMS = int(time.Since(start).Milliseconds())
			code, userErr, auditMsg := serviceNotActivatedResponse(ctx, h.vault, h.store, agent.UserID, serviceType, serviceAlias, req.Service, approveAdapter)
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
		timeout := parseLongPollTimeout(r)
		pa := h.waitForApprovalDecision(r.Context(), req.RequestID, agent.UserID, time.Duration(timeout)*time.Second)
		if pa != nil && pa.Status == "approved" && !time.Now().After(pa.ExpiresAt) {
			h.executeAndRespond(w, r.Context(), pa)
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

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":     "pending",
		"request_id": req.RequestID,
		"audit_id":   auditID,
		"message":    fmt.Sprintf("Approval requested. Waiting up to %ds.", h.cfg.Approval.Timeout),
	})
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
		timeout := parseLongPollTimeout(r)
		entry = h.waitForRequestResolution(r.Context(), requestID, agent.UserID, time.Duration(timeout)*time.Second)
	}

	writeGatewayStatusResponse(w, entry)
}

// waitForRequestResolution subscribes to the event hub and re-fetches the
// audit entry each time an "audit" event fires. Returns as soon as the
// request leaves the "pending" state or the timeout / request context expires.
func (h *GatewayHandler) waitForRequestResolution(ctx context.Context, requestID, userID string, timeout time.Duration) *store.AuditEntry {
	ch, unsub := h.eventHub.Subscribe(userID)
	defer unsub()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			e, err := h.store.GetAuditEntryByRequestID(ctx, requestID, userID)
			if err != nil {
				return &store.AuditEntry{RequestID: requestID, Outcome: "pending"}
			}
			return e
		case <-timer.C:
			e, err := h.store.GetAuditEntryByRequestID(context.Background(), requestID, userID)
			if err != nil {
				return &store.AuditEntry{RequestID: requestID, Outcome: "pending"}
			}
			return e
		case evt, ok := <-ch:
			if !ok {
				e, err := h.store.GetAuditEntryByRequestID(context.Background(), requestID, userID)
				if err != nil {
					return &store.AuditEntry{RequestID: requestID, Outcome: "pending"}
				}
				return e
			}
			if evt.Type != "audit" {
				continue
			}
			e, err := h.store.GetAuditEntryByRequestID(ctx, requestID, userID)
			if err != nil {
				continue
			}
			if e.Outcome != "pending" {
				return e
			}
		}
	}
}

// parseLongPollTimeout extracts the timeout query param, clamped to [1, 120].
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

// waitForApprovalDecision subscribes to the event hub and re-fetches the
// pending approval each time an "audit" or "queue" event fires. Returns as
// soon as the approval leaves the "pending" state or the timeout expires.
func (h *GatewayHandler) waitForApprovalDecision(ctx context.Context, requestID, userID string, timeout time.Duration) *store.PendingApproval {
	ch, unsub := h.eventHub.Subscribe(userID)
	defer unsub()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	fetch := func(c context.Context) *store.PendingApproval {
		pa, err := h.store.GetPendingApproval(c, requestID)
		if err != nil {
			return nil
		}
		return pa
	}

	for {
		select {
		case <-ctx.Done():
			return fetch(context.Background())
		case <-timer.C:
			return fetch(context.Background())
		case evt, ok := <-ch:
			if !ok {
				return fetch(context.Background())
			}
			if evt.Type != "audit" && evt.Type != "queue" {
				continue
			}
			pa := fetch(ctx)
			if pa == nil || pa.Status != "pending" {
				return pa
			}
		}
	}
}

// executeAndRespond atomically claims an approved pending approval, runs the
// adapter, and writes the result as JSON. The atomic claim prevents double-
// execution when multiple code paths race (e.g. wait=true long-poll + /execute).
// Shared by HandleRequest (wait=true) and HandleExecuteApproved.
func (h *GatewayHandler) executeAndRespond(w http.ResponseWriter, ctx context.Context, pa *store.PendingApproval) {
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
	vKey := vaultKeyForServiceAlias(serviceType, alias)

	start := time.Now()
	result, execErr := executeAdapterRequest(ctx, h.vault, h.adapterReg,
		pa.UserID, blob.Service, blob.Action, blob.Params, vKey)
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
		timeout := parseLongPollTimeout(r)
		pa = h.waitForApprovalDecision(r.Context(), requestID, agent.UserID, time.Duration(timeout)*time.Second)
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

	h.executeAndRespond(w, r.Context(), pa)
}

// writeGatewayStatusResponse writes a consistent status payload for a resolved audit entry.
func writeGatewayStatusResponse(w http.ResponseWriter, e *store.AuditEntry) {
	resp := map[string]any{
		"status":     e.Outcome,
		"request_id": e.RequestID,
		"audit_id":   e.ID,
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
// Returns nil if the verifier is a no-op or if verification fails.
func (h *GatewayHandler) runVerification(
	ctx context.Context,
	task *store.Task,
	matchedAction *store.TaskAction,
	req gateway.Request,
	serviceType string,
) *intent.VerificationVerdict {
	var expectedUse, expansionRationale string
	if matchedAction != nil {
		expectedUse = matchedAction.ExpectedUse
		expansionRationale = matchedAction.ExpansionRationale
	}
	var serviceHints string
	if ada, ok := h.adapterReg.Get(serviceType); ok {
		if hinter, ok := ada.(adapters.VerificationHinter); ok {
			serviceHints = hinter.VerificationHints()
		}
	}
	// Chain context: ephemeral tasks use task_id as implicit session;
	// standing tasks require an explicit session_id to scope facts.
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
	chainContextOptOut := task.Lifetime == "standing" && req.SessionID == ""
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
		ChainContextOptOut:  chainContextOptOut,
		ChainContextEnabled: h.cfg.LLM.ChainContext.Enabled,
	})
	return verdict
}

// ── Shared execution logic ────────────────────────────────────────────────────

// executeAdapterRequest fetches the credential from vault and calls the adapter.
// Shared between gateway and approvals handlers.
// vaultKey overrides the default vault key when non-empty (used for aliased services).
func executeAdapterRequest(
	ctx context.Context,
	v vault.Vault,
	reg *adapters.Registry,
	userID, service, action string,
	params map[string]any,
	vaultKey string,
) (*adapters.Result, error) {
	serviceType, _ := parseServiceAlias(service)
	adapter, ok := reg.Get(serviceType)
	if !ok {
		return nil, fmt.Errorf("service %q is not supported", serviceType)
	}

	vKey := vaultKey
	if vKey == "" {
		vKey = vaultKeyForService(serviceType)
	}
	cred, err := v.Get(ctx, userID, vKey)
	if err != nil {
		if errors.Is(err, vault.ErrNotFound) && adapter.ValidateCredential(nil) == nil {
			cred = nil
		} else {
			return nil, err
		}
	}

	result, err := adapter.Execute(ctx, adapters.Request{
		Action:     action,
		Params:     params,
		Credential: cred,
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
	pa := &store.PendingApproval{
		ID:          uuid.New().String(),
		UserID:      userID,
		RequestID:   blob.RequestID,
		AuditID:     auditID,
		RequestBlob: json.RawMessage(blobBytes),
		ExpiresAt:   expiresAt,
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

// vaultKeyForService returns the vault key for a service type (no alias).
// Google services share the "google" base key.
func vaultKeyForService(serviceID string) string {
	if strings.HasPrefix(serviceID, "google.") {
		return "google"
	}
	return serviceID
}

// vaultKeyForServiceAlias returns the vault key for a service type + alias.
// "default" alias maps to the plain base key for backward compatibility.
func vaultKeyForServiceAlias(serviceType, alias string) string {
	base := serviceType
	if strings.HasPrefix(serviceType, "google.") {
		base = "google"
	}
	if alias == "" || alias == "default" {
		return base
	}
	return base + ":" + alias
}

// hasAnyAlias reports whether any vault entry exists for the given service type
// (under any alias). It uses vault.List and checks for matching key prefixes.
func hasAnyAlias(ctx context.Context, v vault.Vault, userID, serviceType string) bool {
	base := vaultKeyForService(serviceType)
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

// serviceNotActivatedResponse returns the error code and message for a missing
// service or alias. It distinguishes ALIAS_NOT_FOUND (service exists under
// other aliases) from SERVICE_NOT_CONFIGURED (service not activated at all).
func serviceNotActivatedResponse(
	ctx context.Context,
	v vault.Vault,
	st store.Store,
	userID, serviceType, serviceAlias, serviceDisplay string,
	adapter adapters.Adapter,
) (code, userErr, auditMsg string) {
	isAlias := false
	if adapter.ValidateCredential(nil) == nil {
		if count, cErr := st.CountServiceMetasByType(ctx, userID, serviceType); cErr == nil && count > 0 {
			isAlias = true
		}
	} else {
		if hasAnyAlias(ctx, v, userID, serviceType) {
			isAlias = true
		}
	}
	if isAlias {
		return "ALIAS_NOT_FOUND",
			fmt.Sprintf("Alias %q does not exist for service %q. Review the available services and aliases via GET /api/skill/catalog.", serviceAlias, serviceType),
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

func adapterSupportsAction(adapter adapters.Adapter, action string) bool {
	for _, a := range adapter.SupportedActions() {
		if a == action {
			return true
		}
	}
	return false
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

