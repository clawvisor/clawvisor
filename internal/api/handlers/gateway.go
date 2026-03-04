package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/callback"
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
	Reason      string         `json:"reason"`
	CallbackURL string         `json:"callback_url"`
}

// GatewayHandler handles POST /api/gateway/request.
type GatewayHandler struct {
	store      store.Store
	vault      vault.Vault
	adapterReg *adapters.Registry
	notifier   notify.Notifier // may be nil if Telegram not configured
	verifier   intent.Verifier
	cfg        config.Config
	logger     *slog.Logger
	baseURL    string
}

func NewGatewayHandler(
	st store.Store,
	v vault.Vault,
	adapterReg *adapters.Registry,
	notifier notify.Notifier,
	verifier intent.Verifier,
	cfg config.Config,
	logger *slog.Logger,
	baseURL string,
) *GatewayHandler {
	return &GatewayHandler{
		store: st, vault: v, adapterReg: adapterReg,
		notifier: notifier, verifier: verifier,
		cfg: cfg, logger: logger, baseURL: baseURL,
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

	// ── Step 2: Hardcoded approval check ─────────────────────────────────────
	hardcoded := RequiresHardcodedApproval(serviceType, req.Action)

	// ── Step 3: Task scope enforcement ───────────────────────────────────────
	if req.TaskID != "" {
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
			writeJSON(w, http.StatusOK, map[string]any{
				"status":     "pending_scope_expansion",
				"task_id":    req.TaskID,
				"request_id": req.RequestID,
				"message":    msg,
			})
			return
		}

		_ = h.store.IncrementTaskRequestCount(ctx, req.TaskID)

		// In scope + auto_execute + not hardcoded → execute directly
		if match.AutoExecute && !hardcoded {
			taskIDPtr := &req.TaskID

			// ── Intent verification ──────────────────────────────────────
			var expectedUse string
			if match.MatchedAction != nil {
				expectedUse = match.MatchedAction.ExpectedUse
			}
			verdict, _ := h.verifier.Verify(ctx, intent.VerifyRequest{
				TaskPurpose: task.Purpose,
				ExpectedUse: expectedUse,
				Service:     req.Service,
				Action:      req.Action,
				Params:      req.Params,
				Reason:      req.Reason,
				TaskID:      req.TaskID,
			})
			if verdict != nil && !verdict.Allow {
				dur := int(time.Since(start).Milliseconds())
				e := baseEntry("verify", "restricted", taskIDPtr)
				e.DurationMS = dur
				e.Verification = intent.MarshalVerdict(verdict)
				if logErr := h.store.LogAudit(ctx, e); logErr != nil {
					h.logger.Warn("audit log failed", "err", logErr)
				}
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
					errMsg := fmt.Sprintf("service %q not activated", req.Service)
					e.ErrorMsg = &errMsg
					if logErr := h.store.LogAudit(ctx, e); logErr != nil {
						h.logger.Warn("audit log failed", "err", logErr)
					}
					writeJSON(w, http.StatusOK, map[string]any{
						"status":     "error",
						"request_id": req.RequestID,
						"audit_id":   auditID,
						"error":      fmt.Sprintf("Service %q is not activated. Connect it in the Clawvisor dashboard before making requests.", req.Service),
						"code":       "SERVICE_NOT_CONFIGURED",
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
						errMsg := fmt.Sprintf("service %q not activated", req.Service)
						e.ErrorMsg = &errMsg
						if logErr := h.store.LogAudit(ctx, e); logErr != nil {
							h.logger.Warn("audit log failed", "err", logErr)
						}
						writeJSON(w, http.StatusOK, map[string]any{
							"status":     "error",
							"request_id": req.RequestID,
							"audit_id":   auditID,
							"error":      fmt.Sprintf("Service %q is not activated. Connect it in the Clawvisor dashboard before making requests.", req.Service),
							"code":       "SERVICE_NOT_CONFIGURED",
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

		// In scope + (!auto_execute || hardcoded) → falls through to per-request approval below
	}

	// ── Step 4: Per-request approval (default path) ──────────────────────────
	// This covers: no task_id, or task in-scope but not auto-execute, or hardcoded approval.

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
	serviceNotActivated := false
	if approveAdapter.ValidateCredential(nil) == nil {
		// Credential-free service: check service_meta for explicit activation.
		if _, metaErr := h.store.GetServiceMeta(ctx, agent.UserID, serviceType, serviceAlias); metaErr != nil {
			serviceNotActivated = true
		}
	} else {
		// Credential-backed service: check vault key.
		vKey := vaultKeyForServiceAlias(serviceType, serviceAlias)
		if _, vaultErr := h.vault.Get(ctx, agent.UserID, vKey); errors.Is(vaultErr, vault.ErrNotFound) {
			serviceNotActivated = true
		}
	}
	if serviceNotActivated {
		e := baseEntry("block", "error", nil)
		e.DurationMS = int(time.Since(start).Milliseconds())
		errMsg := fmt.Sprintf("service %q not activated", req.Service)
		e.ErrorMsg = &errMsg
		if logErr := h.store.LogAudit(ctx, e); logErr != nil {
			h.logger.Warn("audit log failed", "err", logErr)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "error",
			"request_id": req.RequestID,
			"audit_id":   auditID,
			"error":      fmt.Sprintf("Service %q is not activated. Connect it in the Clawvisor dashboard before making requests.", req.Service),
			"code":       "SERVICE_NOT_CONFIGURED",
		})
		return
	}

	// Route to per-request approval.
	middleware.AddLogField(ctx, "decision", "approve")
	middleware.AddLogField(ctx, "outcome", "pending")
	e := baseEntry("approve", "pending", nil)
	if req.TaskID != "" {
		e.TaskID = &req.TaskID
	}
	e.DurationMS = int(time.Since(start).Milliseconds())
	if logErr := h.store.LogAudit(ctx, e); logErr != nil {
		h.logger.Warn("audit log failed", "err", logErr)
	}
	expiresAt := time.Now().Add(time.Duration(h.cfg.Approval.Timeout) * time.Second)
	blob := buildRequestBlob(req, agent)
	reason := ""
	if hardcoded {
		reason = "iMessage send_message always requires human approval"
	}
	if routeErr := h.routeToApproval(ctx, agent.UserID, blob, auditID,
		req.Context.CallbackURL, expiresAt, reason); routeErr != nil {
		h.logger.Warn("route to approval failed", "err", routeErr)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":     "pending",
		"request_id": req.RequestID,
		"audit_id":   auditID,
		"message":    fmt.Sprintf("Approval requested. Waiting up to %ds.", h.cfg.Approval.Timeout),
	})
}

// HandleStatus returns the current status of a gateway request by request_id.
//
// GET /api/gateway/request/{request_id}/status
// Auth: agent bearer token
func (h *GatewayHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
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
	writeGatewayStatusResponse(w, entry)
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

	msgID, err := h.notifier.SendApprovalRequest(ctx, notify.ApprovalRequest{
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
	})
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

func buildRequestBlob(req gateway.Request, agent *store.Agent) *pendingRequestBlob {
	return &pendingRequestBlob{
		Service:     req.Service,
		Action:      req.Action,
		Params:      req.Params,
		UserID:      agent.UserID,
		AgentID:     agent.ID,
		AgentName:   agent.Name,
		RequestID:   req.RequestID,
		Reason:      req.Reason,
		CallbackURL: req.Context.CallbackURL,
	}
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

