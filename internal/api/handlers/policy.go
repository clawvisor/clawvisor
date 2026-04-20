package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/policy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// PolicyHandler serves the user-facing (dashboard + CLI) policy CRUD +
// violation feed for the Stage 3 engine. All endpoints JWT-authed and
// scoped to the requesting user's bridge. Stage 3 M3.
type PolicyHandler struct {
	st     store.Store
	logger *slog.Logger
}

func NewPolicyHandler(st store.Store, logger *slog.Logger) *PolicyHandler {
	return &PolicyHandler{st: st, logger: logger}
}

// -- user helpers ---------------------------------------------------------

// requireOwnedBridge resolves {id} path value, checks ownership, and
// returns the BridgeToken. Writes 403/404 + returns nil on failure.
func (h *PolicyHandler) requireOwnedBridge(w http.ResponseWriter, r *http.Request) *store.BridgeToken {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return nil
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "bridge id required")
		return nil
	}
	bt, err := h.st.GetBridgeTokenByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "bridge not found")
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "bridge load failed")
		}
		return nil
	}
	if bt.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your bridge")
		return nil
	}
	return bt
}

// -- GET /api/plugin/bridges/{id}/policy -----------------------------------

type policyResponse struct {
	BridgeID     string `json:"bridge_id"`
	Version      int    `json:"version"`
	YAML         string `json:"yaml"`
	Enabled      bool   `json:"enabled"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	// CompiledJSON is optional — dashboard normally doesn't need it, but
	// clawvisor CLI does for testing. Include when ?include=compiled.
	CompiledJSON json.RawMessage `json:"compiled,omitempty"`
}

func (h *PolicyHandler) GetPolicy(w http.ResponseWriter, r *http.Request) {
	bt := h.requireOwnedBridge(w, r)
	if bt == nil {
		return
	}
	p, err := h.st.GetPolicyByBridge(r.Context(), bt.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// No policy yet: return a stub so the editor has a starting point.
			writeJSON(w, http.StatusOK, policyResponse{
				BridgeID: bt.ID,
				Version:  0,
				YAML:     defaultPolicyTemplate(),
				Enabled:  false,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "policy load failed")
		return
	}
	resp := policyResponse{
		BridgeID:  p.BridgeID,
		Version:   p.Version,
		YAML:      p.YAML,
		Enabled:   p.Enabled,
		UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if r.URL.Query().Get("include") == "compiled" && p.CompiledJSON != "" {
		resp.CompiledJSON = json.RawMessage(p.CompiledJSON)
	}
	writeJSON(w, http.StatusOK, resp)
}

// -- PUT /api/plugin/bridges/{id}/policy -----------------------------------

type upsertPolicyRequest struct {
	YAML    string `json:"yaml"`
	Enabled bool   `json:"enabled"`
	Comment string `json:"comment,omitempty"`
}

func (h *PolicyHandler) UpsertPolicy(w http.ResponseWriter, r *http.Request) {
	bt := h.requireOwnedBridge(w, r)
	if bt == nil {
		return
	}
	user := middleware.UserFromContext(r.Context())

	var req upsertPolicyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.YAML == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "yaml is required")
		return
	}

	parsed, err := policy.Parse([]byte(req.YAML))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_POLICY", err.Error())
		return
	}
	compiled, err := policy.Compile(parsed)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_POLICY", err.Error())
		return
	}
	// Overwrite bridge_id from the path, not the YAML, so a user can't
	// install a policy on someone else's bridge by editing the YAML.
	compiled.BridgeID = bt.ID
	compiledJSON, err := json.Marshal(compiled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "compiled policy marshal failed")
		return
	}

	p := &store.Policy{
		BridgeID:     bt.ID,
		YAML:         req.YAML,
		CompiledJSON: string(compiledJSON),
		Enabled:      req.Enabled,
		Comment:      req.Comment,
	}
	if user != nil {
		p.AuthorUserID = user.ID
	}
	if err := h.st.UpsertPolicy(r.Context(), p); err != nil {
		h.logger.Error("upsert policy failed", "err", err, "bridge_id", bt.ID)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "save failed")
		return
	}
	writeJSON(w, http.StatusOK, policyResponse{
		BridgeID:  p.BridgeID,
		Version:   p.Version,
		YAML:      p.YAML,
		Enabled:   p.Enabled,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// -- POST /api/plugin/bridges/{id}/policy/validate -------------------------

type validateResponse struct {
	OK       bool              `json:"ok"`
	Error    string            `json:"error,omitempty"`
	Rules    int               `json:"rule_count,omitempty"`
	Default  string            `json:"default_action,omitempty"`
	JudgeOn  bool              `json:"judge_enabled,omitempty"`
	BanOn    bool              `json:"ban_enabled,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
	Rules_   []ruleSummary     `json:"rules,omitempty"`
}

type ruleSummary struct {
	Name    string `json:"name"`
	Action  string `json:"action"`
	Hosts   int    `json:"host_count"`
	Paths   int    `json:"path_count"`
}

func (h *PolicyHandler) ValidatePolicy(w http.ResponseWriter, r *http.Request) {
	bt := h.requireOwnedBridge(w, r)
	if bt == nil {
		return
	}
	var req upsertPolicyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	parsed, err := policy.Parse([]byte(req.YAML))
	if err != nil {
		writeJSON(w, http.StatusOK, validateResponse{OK: false, Error: err.Error()})
		return
	}
	compiled, err := policy.Compile(parsed)
	if err != nil {
		writeJSON(w, http.StatusOK, validateResponse{OK: false, Error: err.Error()})
		return
	}
	resp := validateResponse{
		OK:      true,
		Rules:   len(compiled.Rules),
		Default: string(compiled.DefaultAction),
		JudgeOn: compiled.Judge.Enabled,
		BanOn:   compiled.Ban.Enabled,
	}
	for _, rule := range compiled.Rules {
		resp.Rules_ = append(resp.Rules_, ruleSummary{
			Name:   rule.Name,
			Action: string(rule.Action),
			Hosts:  len(rule.Hosts),
			Paths:  len(rule.Paths),
		})
	}
	// Light warnings the editor can surface without blocking save.
	if len(compiled.Rules) == 0 && compiled.DefaultAction == policy.ActionAllow {
		resp.Warnings = append(resp.Warnings, "no rules and default_action=allow — this policy is a no-op")
	}
	if compiled.Ban.Enabled && !ruleHasBlock(compiled) {
		resp.Warnings = append(resp.Warnings, "ban is enabled but no block rules exist — the ban threshold can never trigger")
	}
	writeJSON(w, http.StatusOK, resp)
}

func ruleHasBlock(c *policy.CompiledPolicy) bool {
	for _, r := range c.Rules {
		if r.Action == policy.ActionBlock {
			return true
		}
	}
	return c.DefaultAction == policy.ActionBlock
}

// -- GET /api/plugin/bridges/{id}/violations -------------------------------

type violationsResponse struct {
	Violations []violationDTO `json:"violations"`
}

type violationDTO struct {
	ID              int64  `json:"id"`
	TS              string `json:"ts"`
	AgentTokenID    string `json:"agent_token_id"`
	RuleName        string `json:"rule_name"`
	Action          string `json:"action"`
	DestinationHost string `json:"destination_host"`
	DestinationPath string `json:"destination_path"`
	Method          string `json:"method"`
	Message         string `json:"message,omitempty"`
}

func (h *PolicyHandler) ListViolations(w http.ResponseWriter, r *http.Request) {
	bt := h.requireOwnedBridge(w, r)
	if bt == nil {
		return
	}
	// Default window: last 7 days. Override with ?since=<unix>.
	since := time.Now().Add(-7 * 24 * time.Hour)
	limit := 200
	records, err := h.st.ListPolicyViolations(r.Context(), bt.ID, since, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "violations load failed")
		return
	}
	out := make([]violationDTO, 0, len(records))
	for _, v := range records {
		out = append(out, violationDTO{
			ID:              v.ID,
			TS:              v.TS.UTC().Format(time.RFC3339),
			AgentTokenID:    v.AgentTokenID,
			RuleName:        v.RuleName,
			Action:          v.Action,
			DestinationHost: v.DestinationHost,
			DestinationPath: v.DestinationPath,
			Method:          v.Method,
			Message:         v.Message,
		})
	}
	writeJSON(w, http.StatusOK, violationsResponse{Violations: out})
}

// -- GET /api/plugin/bridges/{id}/bans + DELETE unban ----------------------

type bansResponse struct {
	Bans []banDTO `json:"bans"`
}

type banDTO struct {
	ID             string `json:"id"`
	AgentTokenID   string `json:"agent_token_id"`
	RuleName       string `json:"rule_name"`
	BannedAt       string `json:"banned_at"`
	ExpiresAt      string `json:"expires_at"`
	ViolationCount int    `json:"violation_count"`
}

func (h *PolicyHandler) ListBans(w http.ResponseWriter, r *http.Request) {
	bt := h.requireOwnedBridge(w, r)
	if bt == nil {
		return
	}
	bans, err := h.st.ListActiveBans(r.Context(), bt.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "bans load failed")
		return
	}
	out := make([]banDTO, 0, len(bans))
	for _, b := range bans {
		out = append(out, banDTO{
			ID:             b.ID,
			AgentTokenID:   b.AgentTokenID,
			RuleName:       b.RuleName,
			BannedAt:       b.BannedAt.UTC().Format(time.RFC3339),
			ExpiresAt:      b.ExpiresAt.UTC().Format(time.RFC3339),
			ViolationCount: b.ViolationCount,
		})
	}
	writeJSON(w, http.StatusOK, bansResponse{Bans: out})
}

// LiftBan handles DELETE /api/plugin/bridges/{id}/bans/{agent}/{rule}.
// Rule segment is the URL-encoded rule name (or "*" for any).
func (h *PolicyHandler) LiftBan(w http.ResponseWriter, r *http.Request) {
	bt := h.requireOwnedBridge(w, r)
	if bt == nil {
		return
	}
	user := middleware.UserFromContext(r.Context())
	agent := r.PathValue("agent")
	rule := r.PathValue("rule")
	if rule == "*" {
		rule = ""
	}
	if agent == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "agent required")
		return
	}
	userID := ""
	if user != nil {
		userID = user.ID
	}
	if err := h.st.LiftAgentBan(r.Context(), bt.ID, agent, rule, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "no active ban")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "lift failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// -- POST .../policy/test --------------------------------------------------
//
// Replay a YAML policy against a set of (host, method, path, agent)
// tuples — lets authors verify a draft before deploying. Stage 3 M4.

type policyTestRequest struct {
	YAML  string           `json:"yaml"`
	Cases []policyTestCase `json:"cases"`
}

type policyTestCase struct {
	Name         string `json:"name,omitempty"`
	Method       string `json:"method"`
	URL          string `json:"url"`
	AgentTokenID string `json:"agent_token_id,omitempty"`
}

type policyTestResponse struct {
	OK     bool                   `json:"ok"`
	Error  string                 `json:"error,omitempty"`
	Cases  []policyTestCaseResult `json:"cases,omitempty"`
}

type policyTestCaseResult struct {
	Name   string `json:"name,omitempty"`
	Action string `json:"action"`
	Rule   string `json:"rule,omitempty"`
}

func (h *PolicyHandler) TestPolicy(w http.ResponseWriter, r *http.Request) {
	bt := h.requireOwnedBridge(w, r)
	if bt == nil {
		return
	}
	var req policyTestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	parsed, err := policy.Parse([]byte(req.YAML))
	if err != nil {
		writeJSON(w, http.StatusOK, policyTestResponse{OK: false, Error: err.Error()})
		return
	}
	compiled, err := policy.Compile(parsed)
	if err != nil {
		writeJSON(w, http.StatusOK, policyTestResponse{OK: false, Error: err.Error()})
		return
	}
	results := make([]policyTestCaseResult, 0, len(req.Cases))
	for _, c := range req.Cases {
		httpReq, rerr := http.NewRequest(c.Method, c.URL, nil)
		if rerr != nil {
			results = append(results, policyTestCaseResult{Name: c.Name, Action: "error"})
			continue
		}
		d := policy.Evaluate(compiled, policy.NewMatchContext(httpReq, c.AgentTokenID))
		res := policyTestCaseResult{Name: c.Name, Action: string(d.Action)}
		if d.Rule != nil {
			res.Rule = d.Rule.Name
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, policyTestResponse{OK: true, Cases: results})
}

// -- POST .../policy/generate ---------------------------------------------
//
// Stage 3 M4 (lite). Suggest a starter policy based on the providers
// the bridge has observed in transcript_events. This is intentionally a
// narrow heuristic — full auto-generation from traffic requires a
// request-level telemetry pipeline we don't have yet (Stage 3 M4 full).

type generatePolicyResponse struct {
	YAML      string   `json:"yaml"`
	Reasoning []string `json:"reasoning,omitempty"`
}

func (h *PolicyHandler) GeneratePolicy(w http.ResponseWriter, r *http.Request) {
	bt := h.requireOwnedBridge(w, r)
	if bt == nil {
		return
	}
	// Recent transcript volume → provider set. The provider list informs
	// which host allow-rules we prepend to the template.
	since := time.Now().Add(-14 * 24 * time.Hour)
	events, err := h.st.ListTranscriptEvents(r.Context(), store.TranscriptEventFilter{
		BridgeID: bt.ID,
		Since:    since,
		Limit:    500,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "transcript read failed")
		return
	}
	providers := map[string]int{}
	for _, e := range events {
		if e.Provider == "" || e.Stream != "llm" {
			continue
		}
		providers[e.Provider]++
	}
	yaml := suggestPolicyFromProviders(providers)
	reasons := []string{
		"Starter template — adjust rules before enabling.",
	}
	if len(providers) > 0 {
		reasons = append(reasons,
			fmt.Sprintf("Observed %d provider(s) in the last 14 days: %s", len(providers), sortedKeyList(providers)))
	} else {
		reasons = append(reasons, "No recent LLM traffic seen — generated template is generic.")
	}
	writeJSON(w, http.StatusOK, generatePolicyResponse{YAML: yaml, Reasoning: reasons})
}

// suggestPolicyFromProviders returns a starter YAML pre-populated with
// allow rules for the provider hosts the bridge has used. Conservative
// default: explicit allows + default flag so unknown destinations are
// reviewable rather than silently blocked.
func suggestPolicyFromProviders(providers map[string]int) string {
	known := map[string][]string{
		"anthropic": {"api.anthropic.com"},
		"openai":    {"api.openai.com"},
		"telegram":  {"api.telegram.org"},
	}
	var rules []string
	seen := map[string]bool{}
	for p := range providers {
		hosts, ok := known[p]
		if !ok {
			continue
		}
		for _, h := range hosts {
			if seen[h] {
				continue
			}
			seen[h] = true
			rules = append(rules, fmt.Sprintf(`    - name: allow_%s
      action: allow
      match:
        hosts: [%q]`, p, h))
		}
	}
	rulesBlock := ""
	if len(rules) > 0 {
		rulesBlock = "\n" + joinLines(rules)
	}
	return fmt.Sprintf(`# Suggested starter policy — review each rule before enabling.
version: 1
name: suggested

rules:
  fast:%s
    - name: block_repo_delete_on_github
      action: block
      match:
        hosts: [api.github.com]
        methods: [DELETE]
        paths: ["/repos/*/*"]
      message: "Repository deletion is not allowed without review."
  judge:
    enabled: false
  default: flag

ban:
  enabled: true
  max_violations: 3
  window: 1h
  ban_duration: 30m
  scope: per_rule
`, rulesBlock)
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}

func sortedKeyList(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Insertion-sort is fine at this size.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}

// defaultPolicyTemplate returns a starter YAML users edit on a fresh
// bridge. Keep it minimal — just enough shape that the dashboard form
// has something to show without blocking save.
func defaultPolicyTemplate() string {
	return fmt.Sprintf(`# Clawvisor policy — starter template.
# See docs/design-proxy-stage3.md for the full schema.
version: 1
name: new-policy

rules:
  fast:
    - name: example_block_github_delete
      action: block
      match:
        hosts: [api.github.com]
        methods: [DELETE]
        paths: ["/repos/*/*"]
      message: "Repository deletion is not allowed."
  judge:
    enabled: false
  default: allow

ban:
  enabled: true
  max_violations: 3
  window: 1h
  ban_duration: 30m
  scope: per_rule
`)
}
