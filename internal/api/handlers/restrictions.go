package handlers

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// RestrictionsHandler manages compatibility for legacy hard-block restrictions.
// Restrictions are now stored as service-kind runtime policy rules with deny action.
type RestrictionsHandler struct {
	st store.Store
}

func NewRestrictionsHandler(st store.Store) *RestrictionsHandler {
	return &RestrictionsHandler{st: st}
}

func serviceRuleToRestriction(rule *store.RuntimePolicyRule) *store.Restriction {
	if rule == nil {
		return nil
	}
	action := strings.TrimSpace(rule.ServiceAction)
	if action == "" {
		action = "*"
	}
	return &store.Restriction{
		ID:        rule.ID,
		UserID:    rule.UserID,
		Service:   rule.Service,
		Action:    action,
		Reason:    rule.Reason,
		CreatedAt: rule.CreatedAt,
	}
}

// List returns all legacy-compatible service restrictions for the authenticated user.
//
// GET /api/restrictions
// Auth: user JWT
func (h *RestrictionsHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	rules, err := h.st.ListRuntimePolicyRules(r.Context(), user.ID, store.RuntimePolicyRuleFilter{
		Kind:    "service",
		Enabled: boolPtr(true),
		Limit:   500,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list restrictions")
		return
	}
	restrictions := make([]*store.Restriction, 0, len(rules))
	for _, rule := range rules {
		if rule == nil || rule.Kind != "service" {
			continue
		}
		restrictions = append(restrictions, serviceRuleToRestriction(rule))
	}
	writeJSON(w, http.StatusOK, restrictions)
}

type createRestrictionRequest struct {
	Service string `json:"service"`
	Action  string `json:"action"`
	Reason  string `json:"reason"`
}

// Create adds a new legacy-compatible restriction backed by a service runtime policy rule.
//
// POST /api/restrictions
// Auth: user JWT
func (h *RestrictionsHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var req createRestrictionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Service = strings.TrimSpace(req.Service)
	req.Action = strings.TrimSpace(req.Action)
	if req.Service == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service is required")
		return
	}
	if req.Action == "" {
		req.Action = "*"
	}

	rule := &store.RuntimePolicyRule{
		ID:            uuid.NewString(),
		UserID:        user.ID,
		Kind:          "service",
		Action:        "deny",
		Service:       req.Service,
		ServiceAction: req.Action,
		Reason:        strings.TrimSpace(req.Reason),
		Source:        "user",
		Enabled:       true,
	}
	if err := h.st.CreateRuntimePolicyRule(r.Context(), rule); err != nil {
		if err == store.ErrConflict {
			writeError(w, http.StatusConflict, "CONFLICT", "restriction already exists for this service/action")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create restriction")
		return
	}

	// Dual-write to the legacy restrictions table so a downgrade to a pre-merge
	// build still sees rules created on this version. Best-effort: a duplicate
	// row in the legacy table is fine, and any other failure is non-fatal.
	if _, err := h.st.CreateRestriction(r.Context(), &store.Restriction{
		ID:      rule.ID,
		UserID:  user.ID,
		Service: req.Service,
		Action:  req.Action,
		Reason:  rule.Reason,
	}); err != nil && err != store.ErrConflict {
		// Do not fail the request — the runtime policy rule is the source of truth.
	}

	created, err := h.st.GetRuntimePolicyRule(r.Context(), rule.ID)
	if err != nil {
		writeJSON(w, http.StatusCreated, serviceRuleToRestriction(rule))
		return
	}
	writeJSON(w, http.StatusCreated, serviceRuleToRestriction(created))
}

// Delete removes a legacy-compatible restriction backed by a service runtime policy rule.
//
// DELETE /api/restrictions/{id}
// Auth: user JWT
func (h *RestrictionsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	rule, err := h.st.GetRuntimePolicyRule(r.Context(), id)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "restriction not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load restriction")
		return
	}
	if rule.UserID != user.ID || rule.Kind != "service" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "restriction not found")
		return
	}

	if err := h.st.DeleteRuntimePolicyRule(r.Context(), id, user.ID); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "restriction not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete restriction")
		return
	}

	// Best-effort: also delete the dual-written legacy row if present.
	_ = h.st.DeleteRestriction(r.Context(), id, user.ID)

	w.WriteHeader(http.StatusNoContent)
}
