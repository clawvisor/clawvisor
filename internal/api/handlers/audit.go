package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/store"
)

// AuditHandler serves the audit log query API.
type AuditHandler struct {
	st store.Store
}

func NewAuditHandler(st store.Store) *AuditHandler {
	return &AuditHandler{st: st}
}

// List returns paginated audit log entries for the authenticated user.
//
// GET /api/audit?service=...&outcome=...&data_origin=...&limit=50&offset=0
// Auth: user JWT
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	q := r.URL.Query()
	filter := store.AuditFilter{
		Service:    q.Get("service"),
		Outcome:    q.Get("outcome"),
		DataOrigin: q.Get("data_origin"),
		TaskID:     q.Get("task_id"),
		Limit:      parseIntQuery(q.Get("limit"), 50),
		Offset:     parseIntQuery(q.Get("offset"), 0),
	}

	entries, total, err := h.st.ListAuditEntries(r.Context(), user.ID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list audit entries")
		return
	}
	if entries == nil {
		entries = []*store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   total,
		"entries": entries,
	})
}

// Get returns a single audit log entry by ID.
//
// GET /api/audit/{id}
// Auth: user JWT
func (h *AuditHandler) Get(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	entry, err := h.st.GetAuditEntry(r.Context(), id, user.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "audit entry not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get audit entry")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// parseIntQuery parses a query string integer, returning defaultVal if missing or invalid.
func parseIntQuery(s string, defaultVal int) int {
	if s == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return defaultVal
	}
	return n
}
