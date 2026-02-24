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

	"github.com/ericlevine/clawvisor/internal/adapters"
	"github.com/ericlevine/clawvisor/internal/adapters/format"
	"github.com/ericlevine/clawvisor/internal/api/middleware"
	"github.com/ericlevine/clawvisor/internal/callback"
	"github.com/ericlevine/clawvisor/internal/config"
	"github.com/ericlevine/clawvisor/internal/filters"
	"github.com/ericlevine/clawvisor/internal/gateway"
	"github.com/ericlevine/clawvisor/internal/llm"
	"github.com/ericlevine/clawvisor/internal/notify"
	"github.com/ericlevine/clawvisor/internal/policy"
	"github.com/ericlevine/clawvisor/internal/safety"
	"github.com/ericlevine/clawvisor/internal/store"
	"github.com/ericlevine/clawvisor/internal/vault"
)

// pendingRequestBlob is stored in pending_approvals.request_blob.
// It contains everything needed to re-execute the request on approval.
type pendingRequestBlob struct {
	Service         string                  `json:"service"`
	Action          string                  `json:"action"`
	Params          map[string]any          `json:"params"`
	UserID          string                  `json:"user_id"`
	AgentID         string                  `json:"agent_id"`
	AgentName       string                  `json:"agent_name"`
	RequestID       string                  `json:"request_id"`
	Reason          string                  `json:"reason"`
	CallbackURL     string                  `json:"callback_url"`
	CallbackKey     string                  `json:"callback_key,omitempty"` // SHA-256(agentToken) — used to HMAC-sign callbacks; not a live credential
	ResponseFilters []policy.ResponseFilter `json:"response_filters,omitempty"`
}

// GatewayHandler handles POST /api/gateway/request.
type GatewayHandler struct {
	store      store.Store
	vault      vault.Vault
	adapterReg *adapters.Registry
	policyReg  *policy.Registry
	notifier   notify.Notifier // may be nil if Telegram not configured
	safety     safety.SafetyChecker
	llmCfg     config.LLMConfig
	cfg        config.Config
	logger     *slog.Logger
	baseURL    string
}

func NewGatewayHandler(
	st store.Store,
	v vault.Vault,
	adapterReg *adapters.Registry,
	policyReg *policy.Registry,
	notifier notify.Notifier,
	safetyChecker safety.SafetyChecker,
	llmCfg config.LLMConfig,
	cfg config.Config,
	logger *slog.Logger,
	baseURL string,
) *GatewayHandler {
	return &GatewayHandler{
		store: st, vault: v, adapterReg: adapterReg, policyReg: policyReg,
		notifier: notifier, safety: safetyChecker, llmCfg: llmCfg, cfg: cfg, logger: logger, baseURL: baseURL,
	}
}

// HandleRequest is the main gateway entry point.
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
	if req.RequestID == "" {
		req.RequestID = uuid.New().String()
	} else {
		// Dedup: if this request_id was already processed, return the existing outcome.
		if existing, err := h.store.GetAuditEntryByRequestID(ctx, req.RequestID, agent.UserID); err == nil {
			writeGatewayStatusResponse(w, existing)
			return
		}
	}

	// Resolve agent role name (policy engine matches on role name, not UUID).
	agentRoleName := ""
	if agent.RoleID != nil {
		if role, err := h.store.GetRole(ctx, *agent.RoleID, agent.UserID); err == nil {
			agentRoleName = role.Name
		}
	}

	paramsSafe, _ := json.Marshal(format.StripSecrets(cloneParams(req.Params)))

	// Pre-resolve the recipient_in_contacts condition if needed.
	// The policy evaluator is a pure function and cannot perform I/O, so we
	// resolve async conditions here and pass them in ResolvedConditions.
	resolvedConditions := map[string]bool{}
	if toEmail, ok := req.Params["to"].(string); ok && toEmail != "" {
		if contactsAdapter, ok := h.adapterReg.Get("google.contacts"); ok {
			if cc, ok := contactsAdapter.(adapters.ContactsChecker); ok {
				if cred, credErr := h.vault.Get(ctx, agent.UserID, "google"); credErr == nil {
					if inContacts, err := cc.IsInContacts(ctx, cred, toEmail); err == nil {
						resolvedConditions["recipient_in_contacts"] = inContacts
					}
				}
			}
		}
	}

	decision := h.policyReg.Evaluate(agent.UserID, policy.EvalRequest{
		Service:            req.Service,
		Action:             req.Action,
		Params:             req.Params,
		AgentRoleID:        agentRoleName,
		ResolvedConditions: resolvedConditions,
	})

	auditID := uuid.New().String()

	// baseEntry builds an AuditEntry with fields common to all outcomes.
	baseEntry := func(outcome string) *store.AuditEntry {
		e := &store.AuditEntry{
			ID:         auditID,
			UserID:     agent.UserID,
			AgentID:    &agent.ID,
			RequestID:  req.RequestID,
			Timestamp:  time.Now().UTC(),
			Service:    req.Service,
			Action:     req.Action,
			ParamsSafe: json.RawMessage(paramsSafe),
			Decision:   string(decision.Decision),
			Outcome:    outcome,
			Reason:     nullableStr(req.Reason),
			DataOrigin: req.Context.DataOrigin,
			ContextSrc: nullableStr(req.Context.Source),
		}
		if decision.PolicyID != "" {
			e.PolicyID = &decision.PolicyID
		}
		if decision.RuleID != "" {
			e.RuleID = &decision.RuleID
		}
		return e
	}

	// iMessage send_message always requires approval — hardcoded, not overridable by policy.
	if req.Service == "apple.imessage" && req.Action == "send_message" &&
		decision.Decision == policy.DecisionExecute {
		decision.Decision = policy.DecisionApprove
		if decision.Reason == "" {
			decision.Reason = "iMessage send_message always requires human approval"
		}
	}

	switch decision.Decision {

	case policy.DecisionBlock:
		e := baseEntry("blocked")
		e.DurationMS = int(time.Since(start).Milliseconds())
		if logErr := h.store.LogAudit(ctx, e); logErr != nil {
			h.logger.Warn("audit log failed", "err", logErr)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":    "blocked",
			"request_id": req.RequestID,
			"audit_id":  auditID,
			"reason":    decision.Reason,
			"policy_id": decision.PolicyID,
		})

	case policy.DecisionApprove:
		// If the service is registered but not activated, route straight to the
		// activation flow — there's no point queuing an approval for a request that
		// will always fail due to missing credentials.
		// (Unknown/unregistered services are allowed through to the queue so that
		// test environments that don't register every adapter still work correctly.)
		if _, ok := h.adapterReg.Get(req.Service); ok {
			vKey := vaultKeyForService(req.Service)
			if _, vaultErr := h.vault.Get(ctx, agent.UserID, vKey); errors.Is(vaultErr, vault.ErrNotFound) {
				e := baseEntry("pending_activation")
				e.DurationMS = int(time.Since(start).Milliseconds())
				errMsg := vaultErr.Error()
				e.ErrorMsg = &errMsg
				if logErr := h.store.LogAudit(ctx, e); logErr != nil {
					h.logger.Warn("audit log failed", "err", logErr)
				}
				expiresAt := time.Now().Add(time.Duration(h.cfg.Approval.Timeout) * time.Second)
				blob := buildRequestBlob(req, agent, nil)
				activateURL := fmt.Sprintf("%s/api/oauth/start?service=%s&pending_request_id=%s",
					h.baseURL, req.Service, req.RequestID)
				denyURL := fmt.Sprintf("%s/api/approvals/%s/deny", h.baseURL, req.RequestID)
				if notifyErr := h.saveAndNotifyActivation(ctx, agent.UserID, blob, auditID,
					req.Context.CallbackURL, expiresAt, req.Service, agent.Name, req.Action,
					activateURL, denyURL); notifyErr != nil {
					h.logger.Warn("activation notification failed", "err", notifyErr)
				}
				writeJSON(w, http.StatusAccepted, map[string]any{
					"status":       "pending_activation",
					"request_id":   req.RequestID,
					"audit_id":     auditID,
					"message":      "Service not activated. Please activate in the Clawvisor dashboard.",
					"code":         "SERVICE_NOT_CONFIGURED",
					"activate_url": activateURL,
				})
				return
			}
		}
		e := baseEntry("pending")
		e.DurationMS = int(time.Since(start).Milliseconds())
		if logErr := h.store.LogAudit(ctx, e); logErr != nil {
			h.logger.Warn("audit log failed", "err", logErr)
		}
		expiresAt := time.Now().Add(time.Duration(h.cfg.Approval.Timeout) * time.Second)
		blob := buildRequestBlob(req, agent, decision.ResponseFilters)
		if routeErr := h.routeToApproval(ctx, agent.UserID, blob, auditID,
			req.Context.CallbackURL, expiresAt, decision.Reason, false, ""); routeErr != nil {
			h.logger.Warn("route to approval failed", "err", routeErr)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":     "pending",
			"request_id": req.RequestID,
			"audit_id":   auditID,
			"message":    fmt.Sprintf("Approval requested. Waiting up to %ds.", h.cfg.Approval.Timeout),
		})

	case policy.DecisionExecute:
		result, filterRecs, execErr := executeAdapterRequest(ctx, h.vault, h.adapterReg,
			agent.UserID, req.Service, req.Action, req.Params, decision.ResponseFilters,
			h.llmFilterFn())
		dur := int(time.Since(start).Milliseconds())

		if execErr != nil {
			if errors.Is(execErr, vault.ErrNotFound) {
				// Service not activated — queue an activation request.
				e := baseEntry("pending_activation")
				e.DurationMS = dur
				errMsg := execErr.Error()
				e.ErrorMsg = &errMsg
				if logErr := h.store.LogAudit(ctx, e); logErr != nil {
					h.logger.Warn("audit log failed", "err", logErr)
				}
				expiresAt := time.Now().Add(time.Duration(h.cfg.Approval.Timeout) * time.Second)
				blob := buildRequestBlob(req, agent, nil)
				activateURL := fmt.Sprintf("%s/api/oauth/start?service=%s&pending_request_id=%s",
					h.baseURL, req.Service, req.RequestID)
				denyURL := fmt.Sprintf("%s/api/approvals/%s/deny", h.baseURL, req.RequestID)
				if notifyErr := h.saveAndNotifyActivation(ctx, agent.UserID, blob, auditID,
					req.Context.CallbackURL, expiresAt, req.Service, agent.Name, req.Action,
					activateURL, denyURL); notifyErr != nil {
					h.logger.Warn("activation notification failed", "err", notifyErr)
				}
				writeJSON(w, http.StatusAccepted, map[string]any{
					"status":       "pending_activation",
					"request_id":   req.RequestID,
					"audit_id":     auditID,
					"message":      "Service not activated. Please activate in the Clawvisor dashboard.",
					"code":         "SERVICE_NOT_CONFIGURED",
					"activate_url": activateURL,
				})
				return
			}
			errMsg := execErr.Error()
			e := baseEntry("error")
			e.DurationMS = dur
			e.ErrorMsg = &errMsg
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			if req.Context.CallbackURL != "" {
				tokenHash := agent.TokenHash
				go func() {
					_ = callback.DeliverResult(context.Background(), req.Context.CallbackURL, &callback.Payload{
						RequestID: req.RequestID, Status: "error", Error: errMsg, AuditID: auditID,
					}, tokenHash)
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

		// LLM safety check (noop if safety disabled).
		safetyResult, _ := h.safety.Check(ctx, agent.UserID, &req, result)
		if !safetyResult.Safe {
			e := baseEntry("pending")
			e.DurationMS = dur
			e.SafetyFlagged = true
			e.SafetyReason = &safetyResult.Reason
			if len(filterRecs) > 0 {
				if b, err := json.Marshal(filterRecs); err == nil {
					e.FiltersApplied = b
				}
			}
			if logErr := h.store.LogAudit(ctx, e); logErr != nil {
				h.logger.Warn("audit log failed", "err", logErr)
			}
			expiresAt := time.Now().Add(time.Duration(h.cfg.Approval.Timeout) * time.Second)
			blob := buildRequestBlob(req, agent, decision.ResponseFilters)
			if routeErr := h.routeToApproval(ctx, agent.UserID, blob, auditID,
				req.Context.CallbackURL, expiresAt, "Safety: "+safetyResult.Reason,
				true, safetyResult.Reason); routeErr != nil {
				h.logger.Warn("route to approval failed (safety)", "err", routeErr)
			}
			writeJSON(w, http.StatusAccepted, map[string]any{
				"status":     "pending",
				"request_id": req.RequestID,
				"audit_id":   auditID,
				"message":    "Safety check flagged this request. Approval requested.",
			})
			return
		}

		// Success — log and return.
		e := baseEntry("executed")
		e.DurationMS = dur
		if len(filterRecs) > 0 {
			if b, err := json.Marshal(filterRecs); err == nil {
				e.FiltersApplied = b
			}
		}
		if logErr := h.store.LogAudit(ctx, e); logErr != nil {
			h.logger.Warn("audit log failed", "err", logErr)
		}
		if req.Context.CallbackURL != "" {
			tokenHash := agent.TokenHash
			go func() {
				_ = callback.DeliverResult(context.Background(), req.Context.CallbackURL, &callback.Payload{
					RequestID: req.RequestID, Status: "executed", Result: result, AuditID: auditID,
				}, tokenHash)
			}()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "executed",
			"request_id": req.RequestID,
			"audit_id":   auditID,
			"result":     result,
		})
	}
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

// ── Shared execution logic ────────────────────────────────────────────────────

// executeAdapterRequest fetches the credential from vault, calls the adapter,
// and applies response filters. Shared between gateway and approvals handlers.
// semanticFn is optional; nil disables LLM semantic filter execution.
func executeAdapterRequest(
	ctx context.Context,
	v vault.Vault,
	reg *adapters.Registry,
	userID, service, action string,
	params map[string]any,
	responseFilters []policy.ResponseFilter,
	semanticFn filters.SemanticApplyFn,
) (*adapters.Result, []filters.FilterRecord, error) {
	adapter, ok := reg.Get(service)
	if !ok {
		return nil, nil, fmt.Errorf("service %q is not supported", service)
	}

	vKey := vaultKeyForService(service)
	cred, err := v.Get(ctx, userID, vKey)
	if err != nil {
		return nil, nil, err // vault.ErrNotFound propagated to caller
	}

	result, err := adapter.Execute(ctx, adapters.Request{
		Action:     action,
		Params:     params,
		Credential: cred,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("adapter %s: %w", service, err)
	}

	// Apply response filters (structural + semantic).
	if len(responseFilters) > 0 {
		result, filterRecs := filters.ApplyContext(ctx, result, responseFilters, semanticFn)
		return result, filterRecs, nil
	}
	return result, nil, nil
}

// llmFilterFn builds a SemanticApplyFn from the current LLM filter config.
// Returns nil if LLM filters are disabled.
func (h *GatewayHandler) llmFilterFn() filters.SemanticApplyFn {
	cfg := h.llmCfg.Filters
	if !cfg.Enabled {
		return nil
	}
	return filters.MakeLLMFilterFn(llm.NewClient(cfg))
}

// ── Approval routing ──────────────────────────────────────────────────────────

// routeToApproval saves a pending_approval record and sends a Telegram notification.
func (h *GatewayHandler) routeToApproval(
	ctx context.Context,
	userID string,
	blob *pendingRequestBlob,
	auditID, callbackURL string,
	expiresAt time.Time,
	policyReason string,
	safetyFlagged bool,
	safetyReason string,
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
	approveURL := fmt.Sprintf("%s/api/approvals/%s/approve", h.baseURL, blob.RequestID)
	denyURL := fmt.Sprintf("%s/api/approvals/%s/deny", h.baseURL, blob.RequestID)

	reason := policyReason
	if safetyFlagged && safetyReason != "" {
		reason = "🔴 Safety check flagged: " + safetyReason
	}

	msgID, err := h.notifier.SendApprovalRequest(ctx, notify.ApprovalRequest{
		PendingID:    pa.ID,
		RequestID:    blob.RequestID,
		UserID:       userID,
		AgentName:    blob.AgentName,
		Service:      blob.Service,
		Action:       blob.Action,
		Params:       blob.Params,
		Reason:       blob.Reason,
		PolicyReason: reason,
		ExpiresIn:    expiresIn,
		ApproveURL:   approveURL,
		DenyURL:      denyURL,
	})
	if err != nil {
		h.logger.Warn("telegram approval notification failed", "err", err)
		return nil // non-fatal: the pending_approval is saved; dashboard can show it
	}

	_ = h.store.UpdatePendingTelegramMsgID(ctx, blob.RequestID, msgID)
	return nil
}

// saveAndNotifyActivation saves a pending_approval and sends an activation notification.
func (h *GatewayHandler) saveAndNotifyActivation(
	ctx context.Context,
	userID string,
	blob *pendingRequestBlob,
	auditID, callbackURL string,
	expiresAt time.Time,
	service, agentName, action, activateURL, denyURL string,
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
		return fmt.Errorf("save pending approval (activation): %w", err)
	}

	if h.notifier == nil {
		return nil
	}

	if err := h.notifier.SendActivationRequest(ctx, notify.ActivationRequest{
		UserID:      userID,
		AgentName:   agentName,
		Service:     service,
		ActivateURL: activateURL,
		DenyURL:     denyURL,
	}); err != nil {
		h.logger.Warn("telegram activation notification failed", "err", err)
	}
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// vaultKeyForService maps a service ID to its vault storage key.
// All Google adapters share the "google" key.
func vaultKeyForService(serviceID string) string {
	if strings.HasPrefix(serviceID, "google.") {
		return "google"
	}
	return serviceID
}

// buildRequestBlob constructs the pendingRequestBlob stored in pending_approvals.
// The callback signing key is the agent's token hash (SHA-256 of the raw token),
// which is already stored in the agents table — storing it here leaks nothing new,
// and it cannot be used to authenticate as the agent.
func buildRequestBlob(req gateway.Request, agent *store.Agent, responseFilters []policy.ResponseFilter) *pendingRequestBlob {
	return &pendingRequestBlob{
		Service:         req.Service,
		Action:          req.Action,
		Params:          req.Params,
		UserID:          agent.UserID,
		AgentID:         agent.ID,
		AgentName:       agent.Name,
		RequestID:       req.RequestID,
		Reason:          req.Reason,
		CallbackURL:     req.Context.CallbackURL,
		CallbackKey:     agent.TokenHash,
		ResponseFilters: responseFilters,
	}
}

// nullableStr returns nil if s is empty, otherwise &s.
func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// cloneParams makes a shallow copy of a params map (to avoid mutating the original).
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
