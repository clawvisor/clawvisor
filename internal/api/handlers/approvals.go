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
	cbDispatch *CallbackDispatcher // bounded callback delivery; may be nil (falls back to inline panic-safe goroutines)
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

// SetCallbackDispatcher wires a bounded callback delivery pool into the
// handler. When unset, callback delivery falls back to a safeGo-wrapped
// inline goroutine — still panic-safe but with no concurrency cap.
func (h *ApprovalsHandler) SetCallbackDispatcher(d *CallbackDispatcher) {
	h.cbDispatch = d
}

// dispatchCallback enqueues a payload for delivery via the bounded
// dispatcher when available, or spawns a panic-safe goroutine otherwise.
func (h *ApprovalsHandler) dispatchCallback(url string, payload *callback.Payload, signingKey string) {
	if url == "" || payload == nil {
		return
	}
	if h.cbDispatch != nil {
		h.cbDispatch.Submit(url, payload, signingKey)
		return
	}
	safeGo(h.logger, "callback delivery (inline)", func() {
		cbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = callback.DeliverResult(cbCtx, url, payload, signingKey)
	})
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
		if errors.Is(err, errApprovalAlreadyResolved) {
			// The row was resolved by a different actor (Telegram deny,
			// expiry sweep, retry) between our load and our CAS. This is
			// a race outcome, not an internal failure — surface a 409 so
			// the dashboard can refresh instead of showing a 500.
			writeError(w, http.StatusConflict, "ALREADY_RESOLVED", "this approval is no longer pending — refresh to see the current state")
			return
		}
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

// revertOrTerminate is the cleanup path for markApproved when post-CAS work
// (task promotion, validation) fails after we already moved status to
// 'approved'. The intuitive "revert to pa.Status" only works when no other
// actor has touched the row since — concurrent /execute can have already
// CAS'd 'approved' → 'executing'. When the revert CAS misses we can't undo
// that; instead, force-resolve the canonical approval record so the user
// dashboard doesn't sit "pending" forever, and let the lease-recovery
// sweeper reclaim the now-orphan 'executing' row on its next pass.
//
// reason is logged for diagnostics; it is NOT passed as the canonical
// approval status (which must be one of "denied" / "expired" — see
// validateApprovalRecordTransition).
func (h *ApprovalsHandler) revertOrTerminate(ctx context.Context, pa *store.PendingApproval, reason string) {
	won, err := h.st.UpdatePendingApprovalStatusFrom(ctx, pa.RequestID, "approved", pa.Status)
	if err != nil {
		h.logger.Error("failed to revert approval status",
			"request_id", pa.RequestID, "reason", reason, "err", err)
		return
	}
	if won {
		return
	}
	// Lost the revert CAS — another actor moved past 'approved' (almost
	// always /execute claiming as 'executing'). Force the canonical
	// approval to a terminal "denied" state so it doesn't stay pending in
	// the dashboard. The pending_approvals row itself will be cleaned up
	// by the lease-recovery sweeper.
	h.logger.Warn("approval revert lost CAS; forcing canonical resolution",
		"request_id", pa.RequestID, "reason", reason)
	h.resolveCanonicalApproval(ctx, pa, "deny", "denied")
}

// errApprovalAlreadyResolved is returned by markApproved when the CAS to
// 'approved' loses to a concurrent resolver (Deny via Telegram, expiry
// sweep, etc.). HTTP callers should map this to 409 Conflict — it is a
// race outcome, not an internal failure.
var errApprovalAlreadyResolved = errors.New("approval is no longer pending")

// markApproved transitions a pending approval to the "approved" state without
// executing it. The agent is expected to call HandleExecuteApproved to claim
// the result. If a callback URL is registered, a notification is sent.
//
// The status update is a CAS from "pending" → "approved", so a concurrent
// Deny against the same request_id can't co-resolve to both states (which
// previously caused the agent to receive both an "approved" and a "denied"
// callback for the same request).
//
// Returns errApprovalAlreadyResolved if the row was resolved by another
// caller before our CAS landed; HTTP handlers translate that to 409.
func (h *ApprovalsHandler) markApproved(ctx context.Context, pa *store.PendingApproval, resolution string) (*store.Task, error) {
	won, err := h.st.UpdatePendingApprovalStatusFrom(ctx, pa.RequestID, "pending", "approved")
	if err != nil {
		h.logger.Error("failed to update approval status", "request_id", pa.RequestID, "err", err)
		return nil, err
	}
	if !won {
		return nil, errApprovalAlreadyResolved
	}
	var promotedTask *store.Task
	switch resolution {
	case "allow_session", "allow_always":
		promotedTask, err = h.promotePendingApprovalToTask(ctx, pa, resolution)
		if err != nil {
			h.revertOrTerminate(ctx, pa, "promotion_failed")
			return nil, err
		}
	case "allow_once":
	default:
		h.revertOrTerminate(ctx, pa, "unsupported_resolution")
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
		h.dispatchCallback(callbackURL, &callback.Payload{
			Type:      "request",
			RequestID: requestID,
			TaskID:    taskID,
			Status:    "approved",
			AuditID:   auditID,
		}, cbKey)
	}
	return promotedTask, nil
}

// DenyByRequestID is the core deny logic, callable from both the HTTP handler
// and the Telegram callback decision consumer.
//
// The status update is a CAS from "pending" → "denied", so a concurrent
// Approve against the same request_id can't co-resolve. If the row has
// already been resolved (approved, denied, or executing), this is a no-op.
func (h *ApprovalsHandler) DenyByRequestID(ctx context.Context, requestID, userID string) error {
	pa, err := h.st.GetPendingApproval(ctx, requestID)
	if err != nil {
		return err
	}
	if pa.UserID != userID {
		return errors.New("not your approval")
	}

	won, err := h.st.UpdatePendingApprovalStatusFrom(ctx, requestID, "pending", "denied")
	if err != nil {
		return err
	}
	if !won {
		// Already resolved by a peer — refuse to repeat the side effects
		// (callback, notifier update, audit) and report no-op to the caller.
		return fmt.Errorf("approval %s is no longer pending", requestID)
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
		h.dispatchCallback(*pa.CallbackURL, &callback.Payload{
			Type:      "request",
			RequestID: requestID,
			Status:    "denied",
			AuditID:   pa.AuditID,
		}, cbKey)
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

	won, err := h.st.UpdatePendingApprovalStatusFrom(r.Context(), requestID, "pending", "denied")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update approval")
		return
	}
	if !won {
		// Concurrent approve/deny race or already-resolved row — refuse to
		// repeat the side effects below.
		writeError(w, http.StatusConflict, "CONFLICT", "approval is no longer pending")
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
		h.dispatchCallback(*pa.CallbackURL, &callback.Payload{
			Type:      "request",
			RequestID: requestID,
			Status:    "denied",
			AuditID:   pa.AuditID,
		}, cbKey)
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
		h.dispatchCallback(*pa.CallbackURL, &callback.Payload{
			Type:      "request",
			RequestID: requestID,
			Status:    outcome,
			Result:    cbResult,
			Error:     cbErr,
			AuditID:   auditID,
		}, cbKey)
	}

	return result, outcome, errMsg
}

// executingLeaseTTL is how long a row may sit in 'executing' before the
// expiry sweeper assumes the executor crashed and recovers it. Five minutes
// comfortably covers the slowest synchronous adapter call (typically <60s)
// while still freeing the user to retry within a single sweep cycle.
const executingLeaseTTL = 5 * time.Minute

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

// processExpiredApproval performs the audit-update + notification + callback
// work for a pending_approvals row that has either timed out (caller already
// deleted the row) or been recovered from a stalled 'executing' state (CAS
// DELETE already happened). reason is included in the structured log so the
// operator can distinguish "user never replied" from "executor crashed
// mid-execution".
//
// The canonical ApprovalRecord is NOT touched here. The two callers each
// know the right thing to do with it:
//   - regular-expired path: canonical record is still 'pending'; the caller
//     flips it to deny/expired inline before invoking us.
//   - stranded-executing path: the user already approved, so the canonical
//     record is already in 'approved'. Flipping it would both lie about the
//     user's decision and fail validateApprovalRecordTransition's
//     pending-only guard, paging on every recovery sweep.
func (h *ApprovalsHandler) processExpiredApproval(ctx context.Context, pa *store.PendingApproval, reason, telegramMsg string) {
	_ = h.st.UpdateAuditOutcome(ctx, pa.AuditID, "timeout", "", 0)
	h.updateNotificationMsg(ctx, "approval", pa.RequestID, pa.UserID, telegramMsg)
	// For the regular expired path the caller relies on us to delete the
	// row; for the stranded path the CAS DELETE already happened, so the
	// best-effort DELETE here is a no-op (zero rows affected).
	_ = h.st.DeletePendingApproval(ctx, pa.RequestID)
	if pa.CallbackURL != nil && *pa.CallbackURL != "" {
		var blob pendingRequestBlob
		_ = json.Unmarshal(pa.RequestBlob, &blob)
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, blob.AgentID)
		// Route through the bounded dispatcher (same as every other
		// resolution path in this handler) instead of calling DeliverResult
		// synchronously. With N expired rows and slow agent endpoints the
		// synchronous version serialized into N × 30s, starving the next
		// sweep tick and the stranded-executing recovery pass.
		h.dispatchCallback(*pa.CallbackURL, &callback.Payload{
			Type:      "request",
			RequestID: pa.RequestID,
			Status:    "timeout",
			AuditID:   pa.AuditID,
		}, cbKey)
	}
	h.decrementNotifierPolling(pa.UserID)
	h.publishQueueAndAudit(pa.UserID, pa.AuditID)
	h.logger.Info("pending approval expired", "request_id", pa.RequestID, "reason", reason)
}

func (h *ApprovalsHandler) expireTimedOut(ctx context.Context) {
	expired, err := h.st.ListExpiredPendingApprovals(ctx)
	if err != nil {
		h.logger.Warn("expiry cleanup: list failed", "err", err)
		return
	}
	for _, pa := range expired {
		// Regular-expired path: the user never replied, so the canonical
		// approval record is still 'pending' and must be moved to a terminal
		// state. The stranded path below skips this because the canonical
		// record is already 'approved' there.
		h.resolveCanonicalApproval(ctx, pa, "deny", "expired")
		h.processExpiredApproval(ctx, pa, "expired", "⏰ <b>Timed out</b> — approval window expired.")
	}
	// Stranded 'executing' rows: claim each via a CAS DELETE before
	// dispatching the timeout callback, otherwise a slow-but-not-crashed
	// executor that finishes between our list and our delete would cause
	// the agent to receive both an "executed" and a "timeout" callback for
	// the same request_id. The CAS WHERE clause guarantees exactly one
	// resolver wins.
	stalled, err := h.st.ListStalledExecutingApprovals(ctx, executingLeaseTTL)
	if err != nil {
		h.logger.Warn("expiry cleanup: stalled-executing list failed", "err", err)
	} else {
		for _, pa := range stalled {
			won, err := h.st.ClaimStalledExecutingApprovalForRecovery(ctx, pa.RequestID, executingLeaseTTL)
			if err != nil {
				h.logger.Warn("expiry cleanup: stalled-executing claim failed", "request_id", pa.RequestID, "err", err)
				continue
			}
			if !won {
				// The executor finished between list and claim — it has
				// already delivered an "executed" callback. Do nothing.
				h.logger.Debug("stalled approval finished before recovery sweep", "request_id", pa.RequestID)
				continue
			}
			h.logger.Warn("recovered stalled executing approval", "request_id", pa.RequestID, "user_id", pa.UserID)
			h.processExpiredApproval(ctx, pa, "stranded", "⏰ <b>Recovered</b> — execution lease expired.")
		}
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
			// See processExpiredApproval — same reason for using the
			// bounded dispatcher instead of synchronous DeliverResult.
			h.dispatchCallback(*task.CallbackURL, &callback.Payload{
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
