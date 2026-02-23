package handlers

import (
	"errors"
	"net/http"

	"github.com/ericlevine/clawvisor/internal/api/middleware"
	"github.com/ericlevine/clawvisor/internal/policy"
	"github.com/ericlevine/clawvisor/internal/store"
)

type PoliciesHandler struct {
	store    store.Store
	registry *policy.Registry
}

func NewPoliciesHandler(st store.Store, reg *policy.Registry) *PoliciesHandler {
	return &PoliciesHandler{store: st, registry: reg}
}

// GET /api/policies
func (h *PoliciesHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	filter := store.PolicyFilter{}
	if r.URL.Query().Get("global") == "true" {
		filter.GlobalOnly = true
	} else if roleSlug := r.URL.Query().Get("role"); roleSlug != "" {
		role, err := h.store.GetRoleByName(r.Context(), roleSlug, user.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "NOT_FOUND", "role not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to look up role")
			return
		}
		filter.RoleID = &role.ID
	}

	records, err := h.store.ListPolicies(r.Context(), user.ID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list policies")
		return
	}
	if records == nil {
		records = []*store.PolicyRecord{}
	}
	writeJSON(w, http.StatusOK, records)
}

// POST /api/policies
func (h *PoliciesHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	var body struct {
		YAML string `json:"yaml"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	rec, conflicts, httpStatus, errMsg := h.parseValidateAndBuild(r, user.ID, body.YAML, "")
	if errMsg != "" {
		writeError(w, httpStatus, "POLICY_ERROR", errMsg)
		return
	}

	created, err := h.store.CreatePolicy(r.Context(), user.ID, rec)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "CONFLICT", "a policy with that id already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create policy")
		return
	}

	// Update registry
	p, _ := policy.Parse([]byte(rec.RulesYAML))
	if p != nil {
		h.registry.Append(user.ID, *p)
	}

	writeJSON(w, http.StatusCreated, policyResponse(created, conflicts))
}

// GET /api/policies/{id}
func (h *PoliciesHandler) Get(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	id := r.PathValue("id")

	rec, err := h.store.GetPolicy(r.Context(), id, user.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "policy not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to get policy")
		return
	}
	writeJSON(w, http.StatusOK, policyResponse(rec, nil))
}

// PUT /api/policies/{id}
func (h *PoliciesHandler) Update(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	id := r.PathValue("id")

	var body struct {
		YAML string `json:"yaml"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	// Fetch the old record before updating so we can remove its rules from the
	// registry by slug, even if the new YAML changes the policy's id field.
	oldRec, err := h.store.GetPolicy(r.Context(), id, user.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "policy not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to get policy")
		return
	}

	rec, conflicts, httpStatus, errMsg := h.parseValidateAndBuild(r, user.ID, body.YAML, id)
	if errMsg != "" {
		writeError(w, httpStatus, "POLICY_ERROR", errMsg)
		return
	}

	updated, err := h.store.UpdatePolicy(r.Context(), id, user.ID, rec)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "policy not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "CONFLICT", "a policy with that slug already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update policy")
		return
	}

	// Remove old rules (by old slug) then append new ones.
	h.registry.Remove(user.ID, oldRec.Slug)
	p, _ := policy.Parse([]byte(rec.RulesYAML))
	if p != nil {
		h.registry.Append(user.ID, *p)
	}

	writeJSON(w, http.StatusOK, policyResponse(updated, conflicts))
}

// DELETE /api/policies/{id}
func (h *PoliciesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	id := r.PathValue("id")

	// Fetch first to get the slug for registry removal
	rec, err := h.store.GetPolicy(r.Context(), id, user.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "policy not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to get policy")
		return
	}

	if err := h.store.DeletePolicy(r.Context(), id, user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete policy")
		return
	}

	h.registry.Remove(user.ID, rec.Slug)
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/policies/validate
func (h *PoliciesHandler) Validate(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	var body struct {
		YAML string `json:"yaml"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	p, parseErr := policy.Parse([]byte(body.YAML))
	if parseErr != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"valid":     false,
			"errors":    []string{parseErr.Error()},
			"conflicts": []policy.Conflict{},
		})
		return
	}

	knownRoles, _ := h.knownRoleIDs(r, user.ID)
	validationErrs := policy.ValidatePolicy(p, knownRoles)
	if len(validationErrs) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"valid":     false,
			"errors":    validationErrs,
			"conflicts": []policy.Conflict{},
		})
		return
	}

	existing, _ := h.store.ListPolicies(r.Context(), user.ID, store.PolicyFilter{})
	existingRules := h.compiledFromRecords(existing, user.ID)
	incoming := policy.Compile([]policy.Policy{*p}, user.ID)
	conflicts := policy.DetectConflicts(incoming, existingRules)

	writeJSON(w, http.StatusOK, map[string]any{
		"valid":     true,
		"errors":    []policy.ValidationError{},
		"conflicts": conflicts,
	})
}

// POST /api/policies/evaluate  (dry-run)
func (h *PoliciesHandler) Evaluate(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	var body struct {
		Service string         `json:"service"`
		Action  string         `json:"action"`
		Params  map[string]any `json:"params"`
		Role    string         `json:"role"` // optional role name for dry-run
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Service == "" || body.Action == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service and action are required")
		return
	}

	// The policy engine matches on role name, not UUID.
	// AgentRoleID = role name (e.g. "researcher"), consistent with CompiledRule.RoleID.
	req := policy.EvalRequest{
		Service:     body.Service,
		Action:      body.Action,
		Params:      body.Params,
		AgentRoleID: body.Role, // role name passed directly
	}

	decision := h.registry.Evaluate(user.ID, req)
	writeJSON(w, http.StatusOK, decision)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

type policyResponseBody struct {
	*store.PolicyRecord
	Conflicts []policy.Conflict `json:"conflicts,omitempty"`
}

func policyResponse(rec *store.PolicyRecord, conflicts []policy.Conflict) policyResponseBody {
	return policyResponseBody{PolicyRecord: rec, Conflicts: conflicts}
}

// parseValidateAndBuild parses YAML, validates it, and builds a PolicyRecord.
// existingID is non-empty on updates (to exclude the current policy from conflict detection).
func (h *PoliciesHandler) parseValidateAndBuild(r *http.Request, userID, yamlStr, existingID string) (*store.PolicyRecord, []policy.Conflict, int, string) {
	if yamlStr == "" {
		return nil, nil, http.StatusBadRequest, "yaml is required"
	}

	p, err := policy.Parse([]byte(yamlStr))
	if err != nil {
		return nil, nil, http.StatusBadRequest, "invalid YAML: " + err.Error()
	}

	knownRoles, roleNameToID := h.knownRoleIDs(r, userID)
	validationErrs := policy.ValidatePolicy(p, knownRoles)
	if len(validationErrs) > 0 {
		return nil, nil, http.StatusBadRequest, validationErrs[0].Error()
	}

	// Resolve role name → role ID
	var roleID *string
	if p.Role != "" {
		if id, ok := roleNameToID[p.Role]; ok {
			roleID = &id
		}
	}

	rec := &store.PolicyRecord{
		Slug:        p.ID,
		Name:        p.Name,
		Description: p.Description,
		RoleID:      roleID,
		RulesYAML:   yamlStr,
	}

	// Conflict detection
	existing, _ := h.store.ListPolicies(r.Context(), userID, store.PolicyFilter{})
	existingRules := h.compiledFromRecordsExcluding(existing, userID, existingID)
	incoming := policy.Compile([]policy.Policy{*p}, userID)
	conflicts := policy.DetectConflicts(incoming, existingRules)

	// Hard error only for opposing_decisions conflicts
	for _, c := range conflicts {
		if c.Type == "opposing_decisions" {
			return nil, conflicts, http.StatusConflict, c.Message
		}
	}

	return rec, conflicts, 0, ""
}

func (h *PoliciesHandler) knownRoleIDs(r *http.Request, userID string) (map[string]bool, map[string]string) {
	roles, err := h.store.ListRoles(r.Context(), userID)
	if err != nil {
		return nil, nil
	}
	known := make(map[string]bool, len(roles))
	nameToID := make(map[string]string, len(roles))
	for _, role := range roles {
		known[role.Name] = true
		nameToID[role.Name] = role.ID
	}
	return known, nameToID
}

func (h *PoliciesHandler) compiledFromRecords(records []*store.PolicyRecord, userID string) []policy.CompiledRule {
	return h.compiledFromRecordsExcluding(records, userID, "")
}

func (h *PoliciesHandler) compiledFromRecordsExcluding(records []*store.PolicyRecord, userID, excludeID string) []policy.CompiledRule {
	var policies []policy.Policy
	for _, rec := range records {
		if rec.ID == excludeID {
			continue
		}
		p, err := policy.Parse([]byte(rec.RulesYAML))
		if err == nil {
			policies = append(policies, *p)
		}
	}
	return policy.Compile(policies, userID)
}
