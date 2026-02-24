package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/ericlevine/clawvisor/internal/adapters"
	"github.com/ericlevine/clawvisor/internal/api/middleware"
	"github.com/ericlevine/clawvisor/internal/callback"
	"github.com/ericlevine/clawvisor/internal/store"
	"github.com/ericlevine/clawvisor/internal/vault"
)

// ApprovalsHandler manages pending approval decisions.
type ApprovalsHandler struct {
	st         store.Store
	vault      vault.Vault
	adapterReg *adapters.Registry
	logger     *slog.Logger
}

func NewApprovalsHandler(st store.Store, v vault.Vault, adapterReg *adapters.Registry, logger *slog.Logger) *ApprovalsHandler {
	return &ApprovalsHandler{st: st, vault: v, adapterReg: adapterReg, logger: logger}
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

	var blob pendingRequestBlob
	if err := json.Unmarshal(pa.RequestBlob, &blob); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "invalid request blob")
		return
	}

	start := time.Now()
	result, _, execErr := executeAdapterRequest(r.Context(), h.vault, h.adapterReg,
		user.ID, blob.Service, blob.Action, blob.Params, blob.ResponseFilters, nil)
	dur := int(time.Since(start).Milliseconds())

	outcome := "executed"
	errMsg := ""
	if execErr != nil {
		outcome = "error"
		errMsg = execErr.Error()
	}

	_ = h.st.UpdateAuditOutcome(r.Context(), pa.AuditID, outcome, errMsg, dur)
	_ = h.st.DeletePendingApproval(r.Context(), requestID)

	if pa.CallbackURL != nil && *pa.CallbackURL != "" {
		var cbResult *adapters.Result
		if execErr == nil {
			cbResult = result
		}
		tok := blob.CallbackKey
		cbErr := errMsg
		go func() {
			_ = callback.DeliverResult(context.Background(), *pa.CallbackURL, &callback.Payload{
				RequestID: requestID,
				Status:    outcome,
				Result:    cbResult,
				Error:     cbErr,
				AuditID:   pa.AuditID,
			}, tok)
		}()
	}

	if execErr != nil {
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
		if task.CallbackURL != nil && *task.CallbackURL != "" {
			_ = callback.DeliverResult(ctx, *task.CallbackURL, &callback.Payload{
				RequestID: task.ID,
				Status:    "task_expired",
			}, "")
		}
		h.logger.Info("task expired", "task_id", task.ID)
	}
}
