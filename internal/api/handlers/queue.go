package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/store"
)

// QueueHandler serves a unified queue of pending approvals and actionable tasks.
type QueueHandler struct {
	st store.Store
}

func NewQueueHandler(st store.Store) *QueueHandler {
	return &QueueHandler{st: st}
}

type queueApproval struct {
	RequestID string         `json:"request_id"`
	AuditID   string         `json:"audit_id"`
	Service   string         `json:"service"`
	Action    string         `json:"action"`
	Params    map[string]any `json:"params"`
	Reason    string         `json:"reason,omitempty"`
}

type queueItem struct {
	Type      string       `json:"type"` // "approval" or "task"
	ID        string       `json:"id"`
	CreatedAt time.Time    `json:"created_at"`
	ExpiresAt *time.Time   `json:"expires_at"`
	Approval  *queueApproval `json:"approval,omitempty"`
	Task      *store.Task    `json:"task,omitempty"`
}

// List returns a merged, time-sorted list of pending approvals and actionable tasks.
//
// GET /api/queue
// Auth: user JWT
func (h *QueueHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	approvals, err := h.st.ListPendingApprovals(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list pending approvals")
		return
	}

	tasks, err := h.st.ListTasks(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list tasks")
		return
	}

	var items []queueItem

	for _, pa := range approvals {
		var blob pendingRequestBlob
		_ = json.Unmarshal(pa.RequestBlob, &blob)

		exp := pa.ExpiresAt
		items = append(items, queueItem{
			Type:      "approval",
			ID:        pa.RequestID,
			CreatedAt: pa.CreatedAt,
			ExpiresAt: &exp,
			Approval: &queueApproval{
				RequestID: pa.RequestID,
				AuditID:   pa.AuditID,
				Service:   blob.Service,
				Action:    blob.Action,
				Params:    blob.Params,
				Reason:    blob.Reason,
			},
		})
	}

	for _, t := range tasks {
		if t.Status != "pending_approval" && t.Status != "pending_scope_expansion" {
			continue
		}
		items = append(items, queueItem{
			Type:      "task",
			ID:        t.ID,
			CreatedAt: t.CreatedAt,
			ExpiresAt: t.ExpiresAt,
			Task:      t,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(items),
		"items": items,
	})
}
