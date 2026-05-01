package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/callback"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// ApprovalsHandler manages pending approval decisions.
type ApprovalsHandler struct {
	st         store.Store
	vault      vault.Vault
	adapterReg *adapters.Registry
	notifier   notify.Notifier // may be nil
	cfg        config.Config
	assessor   taskrisk.Assessor
	logger     *slog.Logger
	eventHub   events.EventHub
}

func NewApprovalsHandler(st store.Store, v vault.Vault, adapterReg *adapters.Registry, notifier notify.Notifier, cfg config.Config, assessor taskrisk.Assessor, logger *slog.Logger, eventHub events.EventHub) *ApprovalsHandler {
	return &ApprovalsHandler{
		st:         st,
		vault:      v,
		adapterReg: adapterReg,
		notifier:   notifier,
		cfg:        cfg,
		assessor:   assessor,
		logger:     logger,
		eventHub:   eventHub,
	}
}

// List returns pending approvals for the authenticated user.
//
// GET /api/approvals
// Auth: user JWT
func (h *ApprovalsHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	entries, err := h.st.ListPendingApprovals(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list pending approvals")
		return
	}
	if entries == nil {
		entries = []*store.PendingApproval{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   len(entries),
		"entries": entries,
	})
}

// Approve marks a pending request as approved. The agent is expected to call
// POST /api/gateway/request/{request_id}/execute to claim the result. If the
// agent registered a callback URL, a notification is also delivered there.
//
// POST /api/approvals/{request_id}/approve
// Auth: user JWT
func (h *ApprovalsHandler) Approve(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	requestID := r.PathValue("request_id")
	pa, err := h.st.GetPendingApproval(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "pending approval not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get pending approval")
		return
	}

	if pa.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your approval")
		return
	}
	if time.Now().After(pa.ExpiresAt) {
		writeError(w, http.StatusGone, "APPROVAL_EXPIRED", "this approval request has expired")
		return
	}

	resolution, ok := h.decodeApproveResolution(w, r)
	if !ok {
		return
	}
	promotedTask, err := h.markApproved(r.Context(), pa, resolution)
	if err != nil {
		h.logger.Error("failed to approve pending request", "request_id", requestID, "resolution", resolution, "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not approve pending request")
		return
	}
	h.publishQueueAndAudit(user.ID, pa.AuditID)

	resp := map[string]any{
		"status":     "approved",
		"request_id": requestID,
		"audit_id":   pa.AuditID,
		"resolution": resolution,
	}
	if promotedTask != nil {
		resp["task_id"] = promotedTask.ID
		resp["task_status"] = promotedTask.Status
		resp["task_lifetime"] = promotedTask.Lifetime
	}
	writeJSON(w, http.StatusOK, resp)
}

// ApproveByRequestID is the core approve logic, callable from both the HTTP handler
// and the Telegram callback decision consumer.
func (h *ApprovalsHandler) ApproveByRequestID(ctx context.Context, requestID, userID string) error {
	pa, err := h.st.GetPendingApproval(ctx, requestID)
	if err != nil {
		return err
	}
	if pa.UserID != userID {
		return errors.New("not your approval")
	}
	if time.Now().After(pa.ExpiresAt) {
		return errors.New("approval expired")
	}

	if _, err := h.markApproved(ctx, pa, "allow_once"); err != nil {
		return err
	}
	h.publishQueueAndAudit(userID, pa.AuditID)
	return nil
}

// markApproved transitions a pending approval to the "approved" state without
// executing it. The agent is expected to call HandleExecuteApproved to claim
// the result. If a callback URL is registered, a notification is sent.
func (h *ApprovalsHandler) markApproved(ctx context.Context, pa *store.PendingApproval, resolution string) (*store.Task, error) {
	if err := h.st.UpdatePendingApprovalStatus(ctx, pa.RequestID, "approved"); err != nil {
		h.logger.Error("failed to update approval status", "request_id", pa.RequestID, "err", err)
		return nil, err
	}
	var promotedTask *store.Task
	var err error
	switch resolution {
	case "allow_session", "allow_always":
		promotedTask, err = h.promotePendingApprovalToTask(ctx, pa, resolution)
		if err != nil {
			if revertErr := h.st.UpdatePendingApprovalStatus(ctx, pa.RequestID, pa.Status); revertErr != nil {
				h.logger.Error("failed to revert approval status after task promotion error", "request_id", pa.RequestID, "err", revertErr)
			}
			return nil, err
		}
	case "allow_once":
	default:
		if revertErr := h.st.UpdatePendingApprovalStatus(ctx, pa.RequestID, pa.Status); revertErr != nil {
			h.logger.Error("failed to revert approval status after invalid resolution", "request_id", pa.RequestID, "err", revertErr)
		}
		return nil, fmt.Errorf("unsupported approval resolution %q", resolution)
	}
	h.resolveCanonicalApproval(ctx, pa, resolution, "approved")
	if err := h.st.UpdateAuditOutcome(ctx, pa.AuditID, "approved", "", 0); err != nil {
		h.logger.Error("failed to update audit outcome", "audit_id", pa.AuditID, "err", err)
	}

	notifyText := "✅ <b>Approved</b> — waiting for agent to execute."
	if promotedTask != nil {
		if promotedTask.Lifetime == "standing" {
			notifyText = "✅ <b>Approved</b> — standing task created and waiting for agent execution."
		} else {
			notifyText = "✅ <b>Approved</b> — session task created and waiting for agent execution."
		}
	}
	h.updateNotificationMsg(ctx, "approval", pa.RequestID, pa.UserID, notifyText)
	h.decrementNotifierPolling(pa.UserID)

	if pa.CallbackURL != nil && *pa.CallbackURL != "" {
		var blob pendingRequestBlob
		if err := json.Unmarshal(pa.RequestBlob, &blob); err != nil {
			return promotedTask, nil
		}
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, blob.AgentID)
		requestID := pa.RequestID
		auditID := pa.AuditID
		callbackURL := *pa.CallbackURL
		taskID := ""
		if promotedTask != nil {
			taskID = promotedTask.ID
		}
		go func() {
			cbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = callback.DeliverResult(cbCtx, callbackURL, &callback.Payload{
				Type:      "request",
				RequestID: requestID,
				TaskID:    taskID,
				Status:    "approved",
				AuditID:   auditID,
			}, cbKey)
		}()
	}
	return promotedTask, nil
}

// DenyByRequestID is the core deny logic, callable from both the HTTP handler
// and the Telegram callback decision consumer.
func (h *ApprovalsHandler) DenyByRequestID(ctx context.Context, requestID, userID string) error {
	pa, err := h.st.GetPendingApproval(ctx, requestID)
	if err != nil {
		return err
	}
	if pa.UserID != userID {
		return errors.New("not your approval")
	}

	var denyBlob pendingRequestBlob
	_ = json.Unmarshal(pa.RequestBlob, &denyBlob)

	h.resolveCanonicalApproval(ctx, pa, "deny", "denied")
	if err := h.st.UpdateAuditOutcome(ctx, pa.AuditID, "denied", "", 0); err != nil {
		h.logger.Error("failed to update audit outcome", "audit_id", pa.AuditID, "err", err)
	}
	if err := h.st.DeletePendingApproval(ctx, requestID); err != nil {
		h.logger.Error("failed to delete pending approval", "request_id", requestID, "err", err)
	}

	h.updateNotificationMsg(ctx, "approval", requestID, pa.UserID, "❌ <b>Denied</b> — request rejected.")
	h.decrementNotifierPolling(pa.UserID)
	h.publishQueueAndAudit(userID, pa.AuditID)

	if pa.CallbackURL != nil && *pa.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, denyBlob.AgentID)
		go func() {
			cbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = callback.DeliverResult(cbCtx, *pa.CallbackURL, &callback.Payload{
				Type:      "request",
				RequestID: requestID,
				Status:    "denied",
				AuditID:   pa.AuditID,
			}, cbKey)
		}()
	}

	return nil
}

// Deny rejects a pending request and notifies the callback URL.
//
// POST /api/approvals/{request_id}/deny
// Auth: user JWT
func (h *ApprovalsHandler) Deny(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	requestID := r.PathValue("request_id")
	pa, err := h.st.GetPendingApproval(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "pending approval not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get pending approval")
		return
	}

	if pa.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your approval")
		return
	}

	var denyBlob pendingRequestBlob
	_ = json.Unmarshal(pa.RequestBlob, &denyBlob)

	h.resolveCanonicalApproval(r.Context(), pa, "deny", "denied")
	if err := h.st.UpdateAuditOutcome(r.Context(), pa.AuditID, "denied", "", 0); err != nil {
		h.logger.Error("failed to update audit outcome", "audit_id", pa.AuditID, "err", err)
	}
	if err := h.st.DeletePendingApproval(r.Context(), requestID); err != nil {
		h.logger.Error("failed to delete pending approval", "request_id", requestID, "err", err)
	}

	h.updateNotificationMsg(r.Context(), "approval", requestID, pa.UserID, "❌ <b>Denied</b> — request rejected.")
	h.decrementNotifierPolling(pa.UserID)
	h.publishQueueAndAudit(user.ID, pa.AuditID)

	if pa.CallbackURL != nil && *pa.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(r.Context(), denyBlob.AgentID)
		go func() {
			cbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = callback.DeliverResult(cbCtx, *pa.CallbackURL, &callback.Payload{
				Type:      "request",
				RequestID: requestID,
				Status:    "denied",
				AuditID:   pa.AuditID,
			}, cbKey)
		}()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "denied",
		"request_id": requestID,
		"audit_id":   pa.AuditID,
	})
}

// executeApproval runs the adapter request for an approved pending approval
// and handles audit logging, notification update, and callback delivery.
func (h *ApprovalsHandler) executeApproval(ctx context.Context, pa *store.PendingApproval) (*adapters.Result, string, string) {
	var blob pendingRequestBlob
	if err := json.Unmarshal(pa.RequestBlob, &blob); err != nil {
		return nil, "error", "invalid request blob"
	}

	serviceType, alias := parseServiceAlias(blob.Service)
	vKey := h.adapterReg.VaultKeyWithAlias(serviceType, alias)

	start := time.Now()
	result, execErr := executeAdapterRequest(ctx, h.vault, h.adapterReg, h.st,
		pa.UserID, blob.Service, blob.Action, blob.Params, vKey)
	dur := int(time.Since(start).Milliseconds())

	outcome := "executed"
	errMsg := ""
	if execErr != nil {
		outcome = "error"
		errMsg = execErr.Error()
	}

	if err := h.st.UpdateAuditOutcome(ctx, pa.AuditID, outcome, errMsg, dur); err != nil {
		h.logger.Error("failed to update audit outcome", "audit_id", pa.AuditID, "err", err)
	}
	if err := h.st.DeletePendingApproval(ctx, pa.RequestID); err != nil {
		h.logger.Error("failed to delete pending approval", "request_id", pa.RequestID, "err", err)
	}

	notifyText := "✅ <b>Approved</b> — request executed."
	if errMsg != "" {
		notifyText = "✅ <b>Approved</b> — execution failed: " + errMsg
	}
	h.updateNotificationMsg(ctx, "approval", pa.RequestID, pa.UserID, notifyText)

	if pa.CallbackURL != nil && *pa.CallbackURL != "" {
		var cbResult *adapters.Result
		if execErr == nil {
			cbResult = result
		}
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, blob.AgentID)
		cbErr := errMsg
		requestID := pa.RequestID
		auditID := pa.AuditID
		go func() {
			cbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = callback.DeliverResult(cbCtx, *pa.CallbackURL, &callback.Payload{
				Type:      "request",
				RequestID: requestID,
				Status:    outcome,
				Result:    cbResult,
				Error:     cbErr,
				AuditID:   auditID,
			}, cbKey)
		}()
	}

	return result, outcome, errMsg
}

// RunExpiryCleanup runs in a background goroutine to expire timed-out approvals.
// Call as: go handler.RunExpiryCleanup(ctx)
func (h *ApprovalsHandler) RunExpiryCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.expireTimedOut(ctx)
		}
	}
}

func (h *ApprovalsHandler) expireTimedOut(ctx context.Context) {
	expired, err := h.st.ListExpiredPendingApprovals(ctx)
	if err != nil {
		h.logger.Warn("expiry cleanup: list failed", "err", err)
		return
	}
	for _, pa := range expired {
		h.resolveCanonicalApproval(ctx, pa, "deny", "expired")
		_ = h.st.UpdateAuditOutcome(ctx, pa.AuditID, "timeout", "", 0)

		// Update the Telegram message before deleting the pending approval.
		h.updateNotificationMsg(ctx, "approval", pa.RequestID, pa.UserID, "⏰ <b>Timed out</b> — approval window expired.")

		_ = h.st.DeletePendingApproval(ctx, pa.RequestID)

		if pa.CallbackURL != nil && *pa.CallbackURL != "" {
			var expiryBlob pendingRequestBlob
			_ = json.Unmarshal(pa.RequestBlob, &expiryBlob)
			cbKey, _ := h.st.GetAgentCallbackSecret(ctx, expiryBlob.AgentID)
			_ = callback.DeliverResult(ctx, *pa.CallbackURL, &callback.Payload{
				Type:      "request",
				RequestID: pa.RequestID,
				Status:    "timeout",
				AuditID:   pa.AuditID,
			}, cbKey)
		}
		h.decrementNotifierPolling(pa.UserID)
		h.publishQueueAndAudit(pa.UserID, pa.AuditID)
		h.logger.Info("pending approval expired", "request_id", pa.RequestID)
	}

	// Expire timed-out tasks.
	expiredTasks, err := h.st.ListExpiredTasks(ctx)
	if err != nil {
		h.logger.Warn("task expiry cleanup: list failed", "err", err)
		return
	}
	for _, task := range expiredTasks {
		_ = h.st.UpdateTaskStatus(ctx, task.ID, "expired")

		h.updateNotificationMsg(ctx, "task", task.ID, task.UserID, "⏰ <b>Task expired</b>")

		if task.CallbackURL != nil && *task.CallbackURL != "" {
			cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
			_ = callback.DeliverResult(ctx, *task.CallbackURL, &callback.Payload{
				Type:   "task",
				TaskID: task.ID,
				Status: "expired",
			}, cbKey)
		}
		h.decrementNotifierPolling(task.UserID)
		h.publishTasksAndQueue(task.UserID)
		h.logger.Info("task expired", "task_id", task.ID)
	}
}

func (h *ApprovalsHandler) resolveCanonicalApproval(ctx context.Context, pa *store.PendingApproval, resolution, status string) {
	if pa == nil {
		return
	}
	approvalID := pa.ApprovalRecordID
	var rec *store.ApprovalRecord
	if approvalID == nil {
		var err error
		rec, err = h.st.GetApprovalRecordByRequestID(ctx, pa.RequestID)
		if err == nil {
			approvalID = &rec.ID
		}
	}
	if approvalID == nil {
		return
	}
	if rec == nil {
		loaded, err := h.st.GetApprovalRecord(ctx, *approvalID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				h.logger.Error("failed to load canonical approval before resolve", "approval_id", *approvalID, "request_id", pa.RequestID, "err", err)
			}
			return
		}
		rec = loaded
	}
	if err := validateApprovalRecordTransition(rec, resolution, status); err != nil {
		h.logger.Error("illegal canonical approval transition", "approval_id", rec.ID, "request_id", pa.RequestID, "kind", rec.Kind, "from_status", rec.Status, "resolution", resolution, "status", status, "err", err)
		return
	}
	if err := h.st.ResolveApprovalRecord(ctx, *approvalID, resolution, status, time.Now().UTC()); err != nil && !errors.Is(err, store.ErrNotFound) {
		h.logger.Error("failed to resolve canonical approval", "approval_id", *approvalID, "request_id", pa.RequestID, "err", err)
	}
}

func (h *ApprovalsHandler) decodeApproveResolution(w http.ResponseWriter, r *http.Request) (string, bool) {
	req := struct {
		Resolution string `json:"resolution"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeDetailedError(w, http.StatusBadRequest, diagnoseJSONError(err))
		return "", false
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return "allow_once", true
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeDetailedError(w, http.StatusBadRequest, diagnoseJSONError(err))
		return "", false
	}
	switch req.Resolution {
	case "", "allow_once":
		return "allow_once", true
	case "allow_session", "allow_always":
		return req.Resolution, true
	default:
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error:     fmt.Sprintf("invalid approval resolution %q", req.Resolution),
			Code:      "INVALID_REQUEST",
			Hint:      "resolution must be one of: allow_once, allow_session, allow_always.",
			Available: []string{"allow_once", "allow_session", "allow_always"},
		})
		return "", false
	}
}

func (h *ApprovalsHandler) promotePendingApprovalToTask(ctx context.Context, pa *store.PendingApproval, resolution string) (*store.Task, error) {
	var blob pendingRequestBlob
	if err := json.Unmarshal(pa.RequestBlob, &blob); err != nil {
		return nil, fmt.Errorf("decode request blob: %w", err)
	}

	lifetime := "session"
	if resolution == "allow_always" {
		lifetime = "standing"
	}
	approvedAt := time.Now().UTC()
	task := &store.Task{
		ID:       uuid.NewSHA1(uuid.NameSpaceURL, []byte("pending-approval:"+pa.RequestID+":"+resolution)).String(),
		UserID:   pa.UserID,
		AgentID:  blob.AgentID,
		Purpose:  derivePromotedTaskPurpose(blob),
		Status:   "active",
		Lifetime: lifetime,
		AuthorizedActions: []store.TaskAction{{
			Service:      blob.Service,
			Action:       blob.Action,
			AutoExecute:  !RequiresHardcodedApproval(blob.Service, blob.Action),
			ExpectedUse:  blob.Reason,
			Verification: "strict",
		}},
		ExpectedUse:    blob.Reason,
		SchemaVersion:  1,
		ApprovedAt:     &approvedAt,
		ApprovalSource: "manual",
	}
	if blob.CallbackURL != "" {
		task.CallbackURL = &blob.CallbackURL
	}
	if lifetime == "session" {
		expiresIn := h.cfg.Task.DefaultExpirySeconds
		if expiresIn <= 0 {
			expiresIn = 1800
		}
		task.ExpiresInSeconds = expiresIn
		expiresAt := approvedAt.Add(time.Duration(expiresIn) * time.Second)
		task.ExpiresAt = &expiresAt
	}
	if h.assessor != nil {
		assessment, err := h.assessor.Assess(ctx, taskrisk.AssessRequest{
			Purpose: task.Purpose,
			AuthorizedActions: []store.TaskAction{{
				Service:      blob.Service,
				Action:       blob.Action,
				AutoExecute:  !RequiresHardcodedApproval(blob.Service, blob.Action),
				ExpectedUse:  blob.Reason,
				Verification: "strict",
			}},
			AgentName: blob.AgentName,
		})
		if err != nil {
			h.logger.Warn("task risk assessment failed for promoted request task", "request_id", pa.RequestID, "err", err)
		} else if assessment != nil {
			task.RiskLevel = assessment.RiskLevel
			task.RiskDetails = taskrisk.MarshalAssessment(assessment)
		}
	}
	if err := h.st.CreateTask(ctx, task); err != nil {
		if errors.Is(err, store.ErrConflict) {
			existing, getErr := h.st.GetTask(ctx, task.ID)
			if getErr == nil {
				return existing, nil
			}
		}
		return nil, fmt.Errorf("create promoted task: %w", err)
	}
	return task, nil
}

func derivePromotedTaskPurpose(blob pendingRequestBlob) string {
	if blob.Reason != "" {
		return blob.Reason
	}
	return fmt.Sprintf("%s:%s", blob.Service, blob.Action)
}

// publishQueueAndAudit publishes SSE events for queue and audit changes.
func (h *ApprovalsHandler) publishQueueAndAudit(userID, auditID string) {
	if h.eventHub == nil {
		return
	}
	h.eventHub.Publish(userID, events.Event{Type: "queue"})
	h.eventHub.Publish(userID, events.Event{Type: "audit", ID: auditID})
}

// publishTasksAndQueue publishes SSE events for tasks and queue changes.
func (h *ApprovalsHandler) publishTasksAndQueue(userID string) {
	if h.eventHub == nil {
		return
	}
	h.eventHub.Publish(userID, events.Event{Type: "tasks"})
	h.eventHub.Publish(userID, events.Event{Type: "queue"})
}

// decrementNotifierPolling calls DecrementPolling on the notifier if it supports it.
func (h *ApprovalsHandler) decrementNotifierPolling(userID string) {
	if h.notifier == nil {
		return
	}
	if pd, ok := h.notifier.(notify.PollingDecrementer); ok {
		pd.DecrementPolling(userID)
	}
}

// updateNotificationMsg updates the Telegram message for a target
// using the notification_messages table.
func (h *ApprovalsHandler) updateNotificationMsg(ctx context.Context, targetType, targetID, userID, text string) {
	if h.notifier == nil {
		return
	}
	msgID, err := h.st.GetNotificationMessage(ctx, targetType, targetID, "telegram")
	if err != nil {
		return
	}
	if err := h.notifier.UpdateMessage(ctx, userID, msgID, text); err != nil {
		h.logger.Warn("telegram message update failed", "err", err, "target_type", targetType, "target_id", targetID)
	}
}
