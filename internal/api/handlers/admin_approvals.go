package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// AdminListApprovals returns every unresolved hold across all users, with
// owner attribution. This is the queue that makes Govern real: an admin — not
// the governed member — can be the approver.
//
// GET /api/admin/approvals
// Auth: admin JWT or instance-admin token
func (h *ApprovalsHandler) AdminListApprovals(w http.ResponseWriter, r *http.Request) {
	entries, err := h.st.ListAllPendingApprovals(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list pending approvals")
		return
	}
	type adminApprovalResponse struct {
		*store.PendingApproval
		OwnerLabel string `json:"owner_label"`
	}
	out := make([]adminApprovalResponse, 0, len(entries))
	for _, pa := range entries {
		out = append(out, adminApprovalResponse{PendingApproval: pa, OwnerLabel: ownerLabelFor(pa.UserID, pa.OwnerEmail)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   len(out),
		"entries": out,
	})
}

// AdminResolveApproval approves or denies ANY user's hold, addressed by the
// hold's globally-unique id (request_id is only unique within a user). The
// existing resolution machinery is reused; the ownership check is lifted to
// "an admin may resolve any hold EXCEPT one raised by their own agent when
// allow_self_approve is false" (04b F7), with the solo-admin exception
// permitted but written to the audit trail as a self-approval.
//
// POST /api/admin/approvals/{id}/resolve
// Body: {"decision":"approve"|"deny","reason"?}
// Auth: admin JWT or instance-admin token
func (h *ApprovalsHandler) AdminResolveApproval(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resolver := middleware.UserFromContext(ctx)
	if resolver == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	pa, err := h.st.GetPendingApprovalByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "pending approval not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get pending approval")
		return
	}

	var body struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	switch body.Decision {
	case "approve", "deny":
	default:
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "decision must be approve or deny")
		return
	}

	// Self-approval enforcement (04b F7). When allow_self_approve is off, an
	// admin may not resolve a hold raised by their OWN agent — except on a
	// solo-admin instance, where no one else could resolve it. That exception
	// is permitted but recorded in the audit trail as a self-approval so it is
	// never silent.
	loggedSelfApproval := false
	if !h.cfg.Approval.AllowSelfApprove && pa.UserID == resolver.ID {
		adminCount, cErr := h.st.CountAdmins(ctx)
		if cErr != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not evaluate self-approval policy")
			return
		}
		soloAdmin := resolver.Role == store.RoleAdmin && adminCount <= 1
		if !soloAdmin {
			writeError(w, http.StatusForbidden, "SELF_APPROVE_FORBIDDEN", "you cannot resolve a hold raised by your own agent; another admin must resolve it")
			return
		}
		loggedSelfApproval = true
	}

	switch body.Decision {
	case "approve":
		if time.Now().After(pa.ExpiresAt) {
			writeError(w, http.StatusGone, "APPROVAL_EXPIRED", "this approval request has expired")
			return
		}
		if _, err := h.markApproved(ctx, pa, "allow_once"); err != nil {
			if errors.Is(err, errApprovalAlreadyResolved) {
				writeError(w, http.StatusConflict, "ALREADY_RESOLVED", "this approval is no longer pending — refresh to see the current state")
				return
			}
			h.logger.ErrorContext(ctx, "admin approve failed", "request_id", pa.RequestID, "err", err)
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not approve pending request")
			return
		}
		h.publishQueueAndAudit(pa.UserID, pa.AuditID)
	case "deny":
		// Reuse the shared deny core against the OWNER's user id (pa.UserID),
		// which satisfies its internal ownership check. A lost CAS / already-
		// resolved row surfaces as an error we map to 409.
		if err := h.DenyByRequestID(ctx, pa.RequestID, pa.UserID, paTaskID(pa)); err != nil {
			h.logger.WarnContext(ctx, "admin deny failed", "request_id", pa.RequestID, "err", err)
			writeError(w, http.StatusConflict, "ALREADY_RESOLVED", "this approval is no longer pending — refresh to see the current state")
			return
		}
	}

	if loggedSelfApproval {
		h.logSoloAdminSelfApproval(ctx, resolver, pa, body.Decision)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "resolved",
		"request_id": pa.RequestID,
		"decision":   body.Decision,
	})
}

// logSoloAdminSelfApproval writes a governance audit row when a solo admin
// resolves a hold raised by their own agent under allow_self_approve=false.
// The row is attributed to the resolving admin (actor_email is server-derived
// from user_id) so the self-approval is visible and non-repudiable.
func (h *ApprovalsHandler) logSoloAdminSelfApproval(ctx context.Context, resolver *store.User, pa *store.PendingApproval, decision string) {
	var agentID *string
	var blob pendingRequestBlob
	if err := json.Unmarshal(pa.RequestBlob, &blob); err == nil && blob.AgentID != "" {
		agentID = &blob.AgentID
	}
	reason := "solo-admin self-approval (allow_self_approve=false)"
	entry := &store.AuditEntry{
		UserID:    resolver.ID,
		AgentID:   agentID,
		RequestID: pa.RequestID,
		Timestamp: time.Now().UTC(),
		Service:   "governance",
		Action:    "self_approve",
		Decision:  decision,
		Outcome:   "self_approved",
		Reason:    &reason,
	}
	if err := h.st.LogAudit(ctx, entry); err != nil {
		h.logger.ErrorContext(ctx, "failed to log solo-admin self-approval", "request_id", pa.RequestID, "err", err)
	}
}
