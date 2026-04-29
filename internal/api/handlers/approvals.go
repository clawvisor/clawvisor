package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/callback"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/pkg/adapters"
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
	logger     *slog.Logger
	eventHub   events.EventHub
}

func NewApprovalsHandler(st store.Store, v vault.Vault, adapterReg *adapters.Registry, notifier notify.Notifier, logger *slog.Logger, eventHub events.EventHub) *ApprovalsHandler {
	return &ApprovalsHandler{st: st, vault: v, adapterReg: adapterReg, notifier: notifier, logger: logger, eventHub: eventHub}
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

	h.markApproved(r.Context(), pa)
	h.publishQueueAndAudit(user.ID, pa.AuditID)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "approved",
		"request_id": requestID,
		"audit_id":   pa.AuditID,
	})
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

	h.markApproved(ctx, pa)
	h.publishQueueAndAudit(userID, pa.AuditID)
	return nil
}

// markApproved transitions a pending approval to the "approved" state without
// executing it. The agent is expected to call HandleExecuteApproved to claim
// the result. If a callback URL is registered, a notification is sent.
func (h *ApprovalsHandler) markApproved(ctx context.Context, pa *store.PendingApproval) {
	if err := h.st.UpdatePendingApprovalStatus(ctx, pa.RequestID, "approved"); err != nil {
		h.logger.Error("failed to update approval status", "request_id", pa.RequestID, "err", err)
	}
	h.resolveCanonicalApproval(ctx, pa, "allow_once", "approved")
	if err := h.st.UpdateAuditOutcome(ctx, pa.AuditID, "approved", "", 0); err != nil {
		h.logger.Error("failed to update audit outcome", "audit_id", pa.AuditID, "err", err)
	}

	h.updateNotificationMsg(ctx, "approval", pa.RequestID, pa.UserID, "✅ <b>Approved</b> — waiting for agent to execute.")
	h.decrementNotifierPolling(pa.UserID)

	if pa.CallbackURL != nil && *pa.CallbackURL != "" {
		var blob pendingRequestBlob
		if err := json.Unmarshal(pa.RequestBlob, &blob); err != nil {
			return
		}
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, blob.AgentID)
		requestID := pa.RequestID
		auditID := pa.AuditID
		callbackURL := *pa.CallbackURL
		go func() {
			cbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = callback.DeliverResult(cbCtx, callbackURL, &callback.Payload{
				Type:      "request",
				RequestID: requestID,
				Status:    "approved",
				AuditID:   auditID,
			}, cbKey)
		}()
	}
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
	if approvalID == nil {
		rec, err := h.st.GetApprovalRecordByRequestID(ctx, pa.RequestID)
		if err == nil {
			approvalID = &rec.ID
		}
	}
	if approvalID == nil {
		return
	}
	if err := h.st.ResolveApprovalRecord(ctx, *approvalID, resolution, status, time.Now().UTC()); err != nil && !errors.Is(err, store.ErrNotFound) {
		h.logger.Error("failed to resolve canonical approval", "approval_id", *approvalID, "request_id", pa.RequestID, "err", err)
	}
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
