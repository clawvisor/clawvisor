package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/adapters"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/callback"
	"github.com/clawvisor/clawvisor/internal/notify"
	"github.com/clawvisor/clawvisor/internal/store"
	"github.com/clawvisor/clawvisor/internal/vault"
)

// ApprovalsHandler manages pending approval decisions.
type ApprovalsHandler struct {
	st         store.Store
	vault      vault.Vault
	adapterReg *adapters.Registry
	notifier   notify.Notifier // may be nil
	logger     *slog.Logger
}

func NewApprovalsHandler(st store.Store, v vault.Vault, adapterReg *adapters.Registry, notifier notify.Notifier, logger *slog.Logger) *ApprovalsHandler {
	return &ApprovalsHandler{st: st, vault: v, adapterReg: adapterReg, notifier: notifier, logger: logger}
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

// Approve executes a pending request and delivers the result to the callback URL.
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

	result, outcome, errMsg := h.executeApproval(r.Context(), pa)

	if outcome == "error" {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "error",
			"request_id": requestID,
			"audit_id":   pa.AuditID,
			"error":      errMsg,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "executed",
		"request_id": requestID,
		"audit_id":   pa.AuditID,
		"result":     result,
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

	h.executeApproval(ctx, pa)
	return nil
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

	_ = h.st.UpdateAuditOutcome(ctx, pa.AuditID, "denied", "", 0)
	_ = h.st.DeletePendingApproval(ctx, requestID)

	h.updateNotificationMsg(ctx, "approval", requestID, pa.UserID, "❌ <b>Denied</b> — request rejected.")
	h.decrementNotifierPolling(pa.UserID)

	if pa.CallbackURL != nil && *pa.CallbackURL != "" {
		tok := denyBlob.CallbackKey
		go func() {
			_ = callback.DeliverResult(context.Background(), *pa.CallbackURL, &callback.Payload{
				RequestID: requestID,
				Status:    "denied",
				AuditID:   pa.AuditID,
			}, tok)
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

	_ = h.st.UpdateAuditOutcome(r.Context(), pa.AuditID, "denied", "", 0)
	_ = h.st.DeletePendingApproval(r.Context(), requestID)

	h.updateNotificationMsg(r.Context(), "approval", requestID, pa.UserID, "❌ <b>Denied</b> — request rejected.")
	h.decrementNotifierPolling(pa.UserID)

	if pa.CallbackURL != nil && *pa.CallbackURL != "" {
		tok := denyBlob.CallbackKey
		go func() {
			_ = callback.DeliverResult(context.Background(), *pa.CallbackURL, &callback.Payload{
				RequestID: requestID,
				Status:    "denied",
				AuditID:   pa.AuditID,
			}, tok)
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

	start := time.Now()
	result, _, execErr := executeAdapterRequest(ctx, h.vault, h.adapterReg,
		pa.UserID, blob.Service, blob.Action, blob.Params, "", blob.ResponseFilters, nil)
	dur := int(time.Since(start).Milliseconds())

	outcome := "executed"
	errMsg := ""
	if execErr != nil {
		outcome = "error"
		errMsg = execErr.Error()
	}

	_ = h.st.UpdateAuditOutcome(ctx, pa.AuditID, outcome, errMsg, dur)
	_ = h.st.DeletePendingApproval(ctx, pa.RequestID)

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
		tok := blob.CallbackKey
		cbErr := errMsg
		requestID := pa.RequestID
		auditID := pa.AuditID
		go func() {
			_ = callback.DeliverResult(context.Background(), *pa.CallbackURL, &callback.Payload{
				RequestID: requestID,
				Status:    outcome,
				Result:    cbResult,
				Error:     cbErr,
				AuditID:   auditID,
			}, tok)
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
		_ = h.st.UpdateAuditOutcome(ctx, pa.AuditID, "timeout", "", 0)

		// Update the Telegram message before deleting the pending approval.
		h.updateNotificationMsg(ctx, "approval", pa.RequestID, pa.UserID, "⏰ <b>Timed out</b> — approval window expired.")

		_ = h.st.DeletePendingApproval(ctx, pa.RequestID)

		if pa.CallbackURL != nil && *pa.CallbackURL != "" {
			var expiryBlob pendingRequestBlob
			_ = json.Unmarshal(pa.RequestBlob, &expiryBlob)
			_ = callback.DeliverResult(ctx, *pa.CallbackURL, &callback.Payload{
				RequestID: pa.RequestID,
				Status:    "timeout",
				AuditID:   pa.AuditID,
			}, expiryBlob.CallbackKey)
		}
		h.decrementNotifierPolling(pa.UserID)
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
			_ = callback.DeliverResult(ctx, *task.CallbackURL, &callback.Payload{
				RequestID: task.ID,
				Status:    "task_expired",
			}, "")
		}
		h.decrementNotifierPolling(task.UserID)
		h.logger.Info("task expired", "task_id", task.ID)
	}
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
