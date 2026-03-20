package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// OverviewHandler serves the bundled GET /api/overview endpoint.
type OverviewHandler struct {
	st store.Store
}

func NewOverviewHandler(st store.Store) *OverviewHandler {
	return &OverviewHandler{st: st}
}

// Get returns queue items, active tasks, and an activity histogram in a single response.
//
// GET /api/overview
// Auth: user JWT
func (h *OverviewHandler) Get(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	// 1. Pending approvals
	approvals, err := h.st.ListPendingApprovals(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list pending approvals")
		return
	}

	// 2. All tasks
	tasks, _, err := h.st.ListTasks(r.Context(), user.ID, store.TaskFilter{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list tasks")
		return
	}

	// Build queue items from approvals + actionable tasks
	var queueItems []queueItem
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
		queueItems = append(queueItems, queueItem{
			Type:      "approval",
			ID:        pa.RequestID,
			CreatedAt: pa.CreatedAt,
			ExpiresAt: &exp,
			Approval:  qa,
		})
	}

	var activeTasks []*store.Task
	for _, t := range tasks {
		sanitizeTaskForResponse(t)

		if t.Status == "pending_approval" || t.Status == "pending_scope_expansion" {
			queueItems = append(queueItems, queueItem{
				Type:      "task",
				ID:        t.ID,
				CreatedAt: t.CreatedAt,
				ExpiresAt: t.ExpiresAt,
				Task:      t,
			})
		}
		if t.Status == "active" {
			activeTasks = append(activeTasks, t)
		}
	}

	// 2b. Connection requests
	connections, err := h.st.ListPendingConnectionRequests(r.Context(), user.ID)
	if err == nil {
		for _, cr := range connections {
			exp := cr.ExpiresAt
			queueItems = append(queueItems, queueItem{
				Type:       "connection",
				ID:         cr.ID,
				CreatedAt:  cr.CreatedAt,
				ExpiresAt:  &exp,
				Connection: cr,
			})
		}
	}

	sort.Slice(queueItems, func(i, j int) bool {
		return queueItems[i].CreatedAt.After(queueItems[j].CreatedAt)
	})

	// 3. Activity histogram (last 60 minutes, 5-minute buckets)
	since := time.Now().Add(-60 * time.Minute)
	activity, err := h.st.AuditActivityBuckets(r.Context(), user.ID, since, 5)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load activity")
		return
	}
	if activity == nil {
		activity = []store.ActivityBucket{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"queue":        queueItems,
		"queue_total":  len(queueItems),
		"active_tasks": activeTasks,
		"activity":     activity,
	})
}
