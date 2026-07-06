package handlers

import (
	"log/slog"
	"net/http"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// AdminHandler serves the instance-admin fleet-wide read surfaces (04b):
// every user's agents, the cross-user audit trail, and the instance cost
// rollup. Every route it backs is mounted under RequireAdminOrToken and is
// OSS-only — it is never mounted in a cloud multi-org composition, where these
// org-blind ListAll* reads would leak across tenants (spec 04b §F5).
//
// The approval queue (list + resolve) lives on ApprovalsHandler because it
// reuses that handler's resolution machinery; see AdminListApprovals /
// AdminResolveApproval.
type AdminHandler struct {
	st     store.Store
	logger *slog.Logger
}

func NewAdminHandler(st store.Store, logger *slog.Logger) *AdminHandler {
	return &AdminHandler{st: st, logger: logger}
}

// automationOwnerLabel is how `_instance`-attributed (Terraform/CI) rows are
// rendered in the admin surfaces — otherwise IaC-managed resources look like
// they belong to a phantom "instance@system…" user.
const automationOwnerLabel = "Terraform / automation"

// ownerLabelFor returns the display label for a row's owner: the automation
// label for `_instance`, the raw email otherwise, or a removed-user marker
// when the owner has been deleted (email blank / actor_email carries the
// email-at-the-time).
func ownerLabelFor(userID, email string) string {
	if userID == store.InstanceUserID {
		return automationOwnerLabel
	}
	if email == "" {
		return "(removed user)"
	}
	return email
}

type adminAgentResponse struct {
	*store.Agent
	OwnerLabel string `json:"owner_label"`
}

// ListAgents returns every agent across all owners, including `_instance`
// (Terraform/CI) rows, with owner attribution.
//
// GET /api/admin/agents
// Auth: admin JWT or instance-admin token
func (h *AdminHandler) ListAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := h.st.ListAllAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list agents")
		return
	}
	out := make([]adminAgentResponse, 0, len(agents))
	for _, a := range agents {
		out = append(out, adminAgentResponse{Agent: a, OwnerLabel: ownerLabelFor(a.UserID, a.OwnerEmail)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":  len(out),
		"agents": out,
	})
}

type adminAuditResponse struct {
	*auditEntryResponse
	OwnerLabel string `json:"owner_label"`
}

// ListAudit returns the cross-user audit trail with the same filters as the
// user-scoped /api/audit plus an optional user_id facet. Departed users
// (whose user_id was nulled on delete) surface via the denormalized
// actor_email, rendered as "<email> (removed)".
//
// GET /api/admin/audit?service=&outcome=&data_origin=&task_id=&agent_id=&user_id=&limit=&offset=
// Auth: admin JWT or instance-admin token
func (h *AdminHandler) ListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseIntQuery(q.Get("limit"), 50)
	if limit > maxListLimit {
		limit = maxListLimit
	}
	includeRuntime := parseBoolQueryDefault(q.Get("include_runtime"), true)
	filter := store.AuditFilter{
		Service:        q.Get("service"),
		Outcome:        q.Get("outcome"),
		DataOrigin:     q.Get("data_origin"),
		TaskID:         q.Get("task_id"),
		AgentID:        q.Get("agent_id"),
		UserID:         q.Get("user_id"),
		IncludeRuntime: &includeRuntime,
		Limit:          limit,
		Offset:         parseIntQuery(q.Get("offset"), 0),
	}

	entries, total, err := h.st.ListAllAuditEvents(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list audit entries")
		return
	}
	respEntries := make([]*adminAuditResponse, 0, len(entries))
	for _, entry := range entries {
		normalized := normalizeAuditEntry(entry)
		label := ownerLabelFor(entry.UserID, entry.ActorEmail)
		// A deleted actor keeps a non-empty actor_email but a nulled user_id
		// (empty here); mark them as removed so the admin can tell live from
		// departed principals.
		if entry.UserID == "" && entry.ActorEmail != "" {
			label = entry.ActorEmail + " (removed)"
		}
		respEntries = append(respEntries, &adminAuditResponse{auditEntryResponse: normalized, OwnerLabel: label})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   total,
		"entries": respEntries,
	})
}

type adminCostUserEntry struct {
	store.InstanceCostByUserEntry
	OwnerLabel string `json:"owner_label"`
}

type adminCostAgentEntry struct {
	store.InstanceCostByAgentEntry
	OwnerLabel string `json:"owner_label"`
}

// Costs returns the single-instance spend rollup over a time window, broken
// down per-user and per-agent. `_instance` (Terraform/CI) spend is included
// and labeled as automation so reports don't under-count IaC-driven usage.
//
// GET /api/admin/costs?window=daily|monthly
// Auth: admin JWT or instance-admin token
func (h *AdminHandler) Costs(w http.ResponseWriter, r *http.Request) {
	window := store.InstanceCostWindow(r.URL.Query().Get("window"))
	switch window {
	case store.InstanceCostWindowDaily, store.InstanceCostWindowMonthly:
	case "":
		window = store.InstanceCostWindowDaily
	default:
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "window must be daily or monthly")
		return
	}

	summary, err := h.st.InstanceCostSummary(r.Context(), window)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not compute cost rollup")
		return
	}

	byUser := make([]adminCostUserEntry, 0, len(summary.ByUser))
	for _, e := range summary.ByUser {
		byUser = append(byUser, adminCostUserEntry{InstanceCostByUserEntry: e, OwnerLabel: ownerLabelFor(e.UserID, e.ActorEmail)})
	}
	byAgent := make([]adminCostAgentEntry, 0, len(summary.ByAgent))
	for _, e := range summary.ByAgent {
		byAgent = append(byAgent, adminCostAgentEntry{InstanceCostByAgentEntry: e, OwnerLabel: ownerLabelFor(e.UserID, e.ActorEmail)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window":             summary.Window,
		"since":              summary.Since,
		"request_count":      summary.RequestCount,
		"input_tokens":       summary.InputTokens,
		"output_tokens":      summary.OutputTokens,
		"cache_read_tokens":  summary.CacheReadTokens,
		"cache_write_tokens": summary.CacheWriteTokens,
		"cost_micros":        summary.CostMicros,
		"unknown_models":     summary.UnknownModels,
		"by_user":            byUser,
		"by_agent":           byAgent,
	})
}
