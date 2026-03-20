package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// QueueHandler serves a unified queue of pending approvals and actionable tasks.
type QueueHandler struct {
	st store.Store
}

func NewQueueHandler(st store.Store) *QueueHandler {
	return &QueueHandler{st: st}
}

type queueApproval struct {
	RequestID    string         `json:"request_id"`
	AuditID      string         `json:"audit_id"`
	Service      string         `json:"service"`
	Action       string         `json:"action"`
	Params       map[string]any `json:"params"`
	Reason       string         `json:"reason,omitempty"`
	Verification map[string]any `json:"verification,omitempty"`
}

type queueItem struct {
	Type       string                `json:"type"` // "approval", "task", or "connection"
	ID         string                `json:"id"`
	CreatedAt  time.Time             `json:"created_at"`
	ExpiresAt  *time.Time            `json:"expires_at"`
	Approval   *queueApproval        `json:"approval,omitempty"`
	Task       *store.Task           `json:"task,omitempty"`
	Connection *store.ConnectionRequest `json:"connection,omitempty"`
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

	tasks, _, err := h.st.ListTasks(r.Context(), user.ID, store.TaskFilter{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list tasks")
		return
	}

	var items []queueItem

	for _, pa := range approvals {
		var blob pendingRequestBlob
		_ = json.Unmarshal(pa.RequestBlob, &blob)

		exp := pa.ExpiresAt
		qa := &queueApproval{
			RequestID: pa.RequestID,
			AuditID:   pa.AuditID,
			Service:   blob.Service,
			Action:    blob.Action,
			Params:    blob.Params,
			Reason:    blob.Reason,
		}
		if blob.Verification != nil {
			b, _ := json.Marshal(blob.Verification)
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				qa.Verification = m
			}
		}
		items = append(items, queueItem{
			Type:      "approval",
			ID:        pa.RequestID,
			CreatedAt: pa.CreatedAt,
			ExpiresAt: &exp,
			Approval:  qa,
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

	// Connection requests
	connections, err := h.st.ListPendingConnectionRequests(r.Context(), user.ID)
	if err == nil {
		for _, cr := range connections {
			exp := cr.ExpiresAt
			items = append(items, queueItem{
				Type:       "connection",
				ID:         cr.ID,
				CreatedAt:  cr.CreatedAt,
				ExpiresAt:  &exp,
				Connection: cr,
			})
		}
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(items),
		"items": items,
	})
}
