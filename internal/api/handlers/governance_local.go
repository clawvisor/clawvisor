package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// GovernanceLocalHandler serves the instance-scoped (local) governance REST
// surface (spec 06a): model policy, spend caps, content policies, task
// policy, and the violation log. Request/response JSON bodies are
// BYTE-IDENTICAL to cloud's org-scoped governance handler (the org segment
// is simply dropped from the path) so the Terraform provider's governance
// resources are schema-identical across OSS and cloud (PRD §8).
//
// All routes are admin-gated at the mux (RequireAdminOrToken — JWT admin or
// an instance-admin API token). Write validation mirrors cloud: model ids
// must be provider-qualified; regex patterns must compile and be <=256
// chars.
type GovernanceLocalHandler struct {
	st     store.Store
	logger *slog.Logger
}

// NewGovernanceLocalHandler constructs the handler.
func NewGovernanceLocalHandler(st store.Store, logger *slog.Logger) *GovernanceLocalHandler {
	return &GovernanceLocalHandler{st: st, logger: logger}
}

// actor returns the user id to stamp as created_by. Token-authenticated
// writes are attributed to the `_instance` system user (the middleware
// resolves UserFromContext to it), which is a valid users row.
func (h *GovernanceLocalHandler) actor(r *http.Request) string {
	if u := middleware.UserFromContext(r.Context()); u != nil && u.ID != "" {
		return u.ID
	}
	return store.InstanceUserID
}

// ── Model policy ──────────────────────────────────────────────────────────────

// GetModelPolicy returns the active model policy. 404 when none set.
//
// GET /api/governance/model_policy
func (h *GovernanceLocalHandler) GetModelPolicy(w http.ResponseWriter, r *http.Request) {
	mp, err := h.st.GetActiveInstanceModelPolicy(r.Context())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "no model policy set")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load model policy")
		return
	}
	writeJSON(w, http.StatusOK, modelPolicyBody(mp))
}

// PutModelPolicy upserts the model policy (append-only server-side).
//
// PUT /api/governance/model_policy  body: {mode, models[]}
func (h *GovernanceLocalHandler) PutModelPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode   string   `json:"mode"`
		Models []string `json:"models"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Mode != "allow" && body.Mode != "deny" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "mode must be \"allow\" or \"deny\"")
		return
	}
	if len(body.Models) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "models must not be empty")
		return
	}
	for _, m := range body.Models {
		if !isProviderQualifiedModel(m) {
			writeError(w, http.StatusUnprocessableEntity, "INVALID_MODEL_ID",
				"model ids must be provider-qualified (\"provider/model\"): "+m)
			return
		}
	}
	p := &store.InstanceModelPolicy{Mode: body.Mode, Models: body.Models, CreatedBy: h.actor(r)}
	if err := h.st.PutInstanceModelPolicy(r.Context(), p); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save model policy")
		return
	}
	writeJSON(w, http.StatusOK, modelPolicyBody(p))
}

// DeleteModelPolicy clears the model policy.
//
// DELETE /api/governance/model_policy
func (h *GovernanceLocalHandler) DeleteModelPolicy(w http.ResponseWriter, r *http.Request) {
	if err := h.st.ClearInstanceModelPolicy(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not clear model policy")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func modelPolicyBody(p *store.InstanceModelPolicy) map[string]any {
	models := p.Models
	if models == nil {
		models = []string{}
	}
	return map[string]any{"mode": p.Mode, "models": models}
}

// ── Spend caps ──────────────────────────────────────────────────────────────

// ListSpendCaps returns all configured spend caps.
//
// GET /api/governance/spend_caps  →  {spend_caps: [{window, cap_micros, enforcement}]}
func (h *GovernanceLocalHandler) ListSpendCaps(w http.ResponseWriter, r *http.Request) {
	caps, err := h.st.ListInstanceSpendCaps(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list spend caps")
		return
	}
	out := make([]map[string]any, 0, len(caps))
	for _, c := range caps {
		out = append(out, spendCapBody(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"spend_caps": out})
}

// PutSpendCap upserts the cap for a window (path param).
//
// PUT /api/governance/spend_caps/{window}  body: {cap_micros, enforcement}
func (h *GovernanceLocalHandler) PutSpendCap(w http.ResponseWriter, r *http.Request) {
	window := r.PathValue("window")
	if window != "daily" && window != "monthly" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "window must be \"daily\" or \"monthly\"")
		return
	}
	var body struct {
		CapMicros   int64  `json:"cap_micros"`
		Enforcement string `json:"enforcement"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.CapMicros <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "cap_micros must be positive")
		return
	}
	enforcement := body.Enforcement
	if enforcement == "" {
		enforcement = "soft"
	}
	if enforcement != "soft" && enforcement != "hard" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "enforcement must be \"soft\" or \"hard\"")
		return
	}
	c := &store.InstanceSpendCap{
		WindowKind: window, CapMicros: body.CapMicros, Enforcement: enforcement, CreatedBy: h.actor(r),
	}
	if err := h.st.PutInstanceSpendCap(r.Context(), c); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save spend cap")
		return
	}
	writeJSON(w, http.StatusOK, spendCapBody(c))
}

// DeleteSpendCap clears the cap for a window.
//
// DELETE /api/governance/spend_caps/{window}
func (h *GovernanceLocalHandler) DeleteSpendCap(w http.ResponseWriter, r *http.Request) {
	window := r.PathValue("window")
	if err := h.st.DeleteInstanceSpendCap(r.Context(), window); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "no spend cap for window")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete spend cap")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func spendCapBody(c *store.InstanceSpendCap) map[string]any {
	return map[string]any{
		"window":      c.WindowKind,
		"cap_micros":  c.CapMicros,
		"enforcement": c.Enforcement,
	}
}

// ── Content policies ──────────────────────────────────────────────────────────

// ListContentPolicies returns all content policies.
//
// GET /api/governance/content_policies  →  {content_policies: [...]}
func (h *GovernanceLocalHandler) ListContentPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := h.st.ListInstanceContentPolicies(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list content policies")
		return
	}
	out := make([]map[string]any, 0, len(policies))
	for _, p := range policies {
		out = append(out, contentPolicyBody(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"content_policies": out})
}

// CreateContentPolicy creates a content policy.
//
// POST /api/governance/content_policies  body: {name, pattern, pattern_kind, action, block_message, enabled}
func (h *GovernanceLocalHandler) CreateContentPolicy(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeContentPolicyBody(w, r)
	if !ok {
		return
	}
	p := &store.InstanceContentPolicy{
		Name: body.Name, Pattern: body.Pattern, PatternKind: body.PatternKind,
		Action: body.Action, BlockMessage: body.BlockMessage, Enabled: body.enabledOrDefault(),
		CreatedBy: h.actor(r),
	}
	if err := h.st.CreateInstanceContentPolicy(r.Context(), p); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create content policy")
		return
	}
	writeJSON(w, http.StatusCreated, contentPolicyBody(p))
}

// UpdateContentPolicy updates a content policy by id.
//
// PUT /api/governance/content_policies/{cpid}
func (h *GovernanceLocalHandler) UpdateContentPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cpid")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "content policy id is required")
		return
	}
	body, ok := decodeContentPolicyBody(w, r)
	if !ok {
		return
	}
	p := &store.InstanceContentPolicy{
		ID: id, Name: body.Name, Pattern: body.Pattern, PatternKind: body.PatternKind,
		Action: body.Action, BlockMessage: body.BlockMessage, Enabled: body.enabledOrDefault(),
	}
	if err := h.st.UpdateInstanceContentPolicy(r.Context(), p); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "content policy not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update content policy")
		return
	}
	got, err := h.st.GetInstanceContentPolicy(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not reload content policy")
		return
	}
	writeJSON(w, http.StatusOK, contentPolicyBody(got))
}

// DeleteContentPolicy removes a content policy by id.
//
// DELETE /api/governance/content_policies/{cpid}
func (h *GovernanceLocalHandler) DeleteContentPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cpid")
	if err := h.st.DeleteInstanceContentPolicy(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "content policy not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete content policy")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type contentPolicyInput struct {
	Name         string `json:"name"`
	Pattern      string `json:"pattern"`
	PatternKind  string `json:"pattern_kind"`
	Action       string `json:"action"`
	BlockMessage string `json:"block_message"`
	Enabled      *bool  `json:"enabled"`
}

func (b contentPolicyInput) enabledOrDefault() bool {
	if b.Enabled == nil {
		return true
	}
	return *b.Enabled
}

// decodeContentPolicyBody decodes + validates a content policy request.
func decodeContentPolicyBody(w http.ResponseWriter, r *http.Request) (contentPolicyInput, bool) {
	var body contentPolicyInput
	if !decodeJSON(w, r, &body) {
		return body, false
	}
	if strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name is required")
		return body, false
	}
	if body.Pattern == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "pattern is required")
		return body, false
	}
	if body.PatternKind != "regex" && body.PatternKind != "keyword" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "pattern_kind must be \"regex\" or \"keyword\"")
		return body, false
	}
	if body.Action != "block" && body.Action != "flag" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "action must be \"block\" or \"flag\"")
		return body, false
	}
	if body.PatternKind == "regex" {
		if len(body.Pattern) > 256 {
			writeError(w, http.StatusUnprocessableEntity, "INVALID_PATTERN", "regex pattern must be <=256 chars")
			return body, false
		}
		if _, err := regexp.Compile(body.Pattern); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "INVALID_PATTERN", "regex pattern does not compile")
			return body, false
		}
	}
	return body, true
}

func contentPolicyBody(p *store.InstanceContentPolicy) map[string]any {
	return map[string]any{
		"id":            p.ID,
		"name":          p.Name,
		"pattern":       p.Pattern,
		"pattern_kind":  p.PatternKind,
		"action":        p.Action,
		"block_message": p.BlockMessage,
		"enabled":       p.Enabled,
	}
}

// ── Task policy ──────────────────────────────────────────────────────────────

// GetTaskPolicy returns the active task policy. 404 when none set.
//
// GET /api/governance/task_policy
func (h *GovernanceLocalHandler) GetTaskPolicy(w http.ResponseWriter, r *http.Request) {
	tp, err := h.st.GetActiveInstanceTaskPolicy(r.Context())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "no task policy set")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load task policy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"guidance": tp.Guidance})
}

// PutTaskPolicy upserts the task policy (append-only server-side).
//
// PUT /api/governance/task_policy  body: {guidance}
func (h *GovernanceLocalHandler) PutTaskPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Guidance string `json:"guidance"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Guidance) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "guidance is required")
		return
	}
	p := &store.InstanceTaskPolicy{Guidance: body.Guidance, CreatedBy: h.actor(r)}
	if err := h.st.PutInstanceTaskPolicy(r.Context(), p); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save task policy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"guidance": p.Guidance})
}

// DeleteTaskPolicy clears the task policy.
//
// DELETE /api/governance/task_policy
func (h *GovernanceLocalHandler) DeleteTaskPolicy(w http.ResponseWriter, r *http.Request) {
	if err := h.st.ClearInstanceTaskPolicy(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not clear task policy")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Violations ──────────────────────────────────────────────────────────────

// ListViolations returns the most recent policy violations.
//
// GET /api/governance/violations  →  {violations: [...]}
func (h *GovernanceLocalHandler) ListViolations(w http.ResponseWriter, r *http.Request) {
	violations, err := h.st.ListInstancePolicyViolations(r.Context(), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list violations")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"violations": violations})
}

// isProviderQualifiedModel reports whether a model id carries a provider
// prefix ("provider/model"). Bare names are rejected — same rule as cloud
// (010_governance_policies.sql header). The provider segment and rest must
// both be non-empty.
func isProviderQualifiedModel(m string) bool {
	i := strings.IndexByte(m, '/')
	return i > 0 && i < len(m)-1
}
