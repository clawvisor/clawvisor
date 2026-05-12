package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type runtimeToolControlResponse struct {
	AgentID           string                     `json:"agent_id"`
	ToolName          string                     `json:"tool_name"`
	Action            string                     `json:"action"`
	RuleID            string                     `json:"rule_id,omitempty"`
	Source            string                     `json:"source"`
	LastSeenAt        *time.Time                 `json:"last_seen_at,omitempty"`
	AdvancedRuleCount int                        `json:"advanced_rule_count"`
	AdvancedRules     []*store.RuntimePolicyRule `json:"advanced_rules"`
}

func (h *RuntimeHandler) ListToolControls(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "agent_id is required")
		return
	}
	if _, err := loadUserAgent(r.Context(), h.st, user.ID, agentID); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "agent_id must belong to the current user")
		return
	}

	controls := map[string]*runtimeToolControlResponse{}
	ensure := func(name string) *runtimeToolControlResponse {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil
		}
		ctrl := controls[name]
		if ctrl == nil {
			ctrl = &runtimeToolControlResponse{
				AgentID:    agentID,
				ToolName:   name,
				Action:     "allow",
				Source:     "default",
				LastSeenAt: nil,
			}
			controls[name] = ctrl
		}
		return ctrl
	}

	entries, _, err := h.st.ListAuditEntries(r.Context(), user.ID, store.AuditFilter{
		AgentID: agentID,
		Limit:   500,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list observed tools")
		return
	}
	for _, entry := range entries {
		var params map[string]any
		if len(entry.ParamsSafe) > 0 {
			_ = json.Unmarshal(entry.ParamsSafe, &params)
		}
		switch readString(params["event"]) {
		case "lite_proxy.endpoint_call":
			for _, name := range readStringSlice(params["available_tools"]) {
				ctrl := ensure(name)
				if ctrl == nil {
					continue
				}
				ctrl.Source = preferToolControlSource(ctrl.Source, "request")
				if ctrl.LastSeenAt == nil || entry.Timestamp.After(*ctrl.LastSeenAt) {
					ts := entry.Timestamp
					ctrl.LastSeenAt = &ts
				}
			}
		case "lite_proxy.tool_use_inspected":
			ctrl := ensure(readString(params["tool_name"]))
			if ctrl == nil {
				continue
			}
			ctrl.Source = preferToolControlSource(ctrl.Source, "observed")
			if ctrl.LastSeenAt == nil || entry.Timestamp.After(*ctrl.LastSeenAt) {
				ts := entry.Timestamp
				ctrl.LastSeenAt = &ts
			}
		}
	}

	rules, err := h.st.ListRuntimePolicyRules(r.Context(), user.ID, store.RuntimePolicyRuleFilter{
		AgentID: agentID,
		Kind:    "tool",
		Limit:   500,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list tool rules")
		return
	}
	for _, rule := range rules {
		if rule == nil || strings.TrimSpace(rule.ToolName) == "" {
			continue
		}
		ctrl := ensure(rule.ToolName)
		if ctrl == nil {
			continue
		}
		if isSimpleToolControlRule(rule) {
			if rule.Enabled && (ctrl.RuleID == "" || rule.AgentID != nil) {
				ctrl.Action = normalizeToolControlAction(rule.Action)
				ctrl.RuleID = rule.ID
				ctrl.Source = "rule"
			}
			continue
		}
		ctrl.AdvancedRuleCount++
		ctrl.AdvancedRules = append(ctrl.AdvancedRules, rule)
	}

	out := make([]*runtimeToolControlResponse, 0, len(controls))
	for _, ctrl := range controls {
		out = append(out, ctrl)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].ToolName) < strings.ToLower(out[j].ToolName)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": out,
		"total":   len(out),
	})
}

func (h *RuntimeHandler) UpsertToolControl(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	var body struct {
		AgentID  string `json:"agent_id"`
		ToolName string `json:"tool_name"`
		Action   string `json:"action"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	agentID := strings.TrimSpace(body.AgentID)
	toolName := strings.TrimSpace(body.ToolName)
	action := normalizeToolControlAction(body.Action)
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "agent_id is required")
		return
	}
	if toolName == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "tool_name is required")
		return
	}
	if action == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "action must be allow, review, or deny")
		return
	}
	if _, err := loadUserAgent(r.Context(), h.st, user.ID, agentID); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "agent_id must belong to the current user")
		return
	}

	rules, err := h.st.ListRuntimePolicyRules(r.Context(), user.ID, store.RuntimePolicyRuleFilter{
		AgentID: agentID,
		Kind:    "tool",
		Limit:   500,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list tool rules")
		return
	}
	for _, rule := range rules {
		if rule == nil || rule.AgentID == nil || *rule.AgentID != agentID || rule.ToolName != toolName || !isSimpleToolControlRule(rule) {
			continue
		}
		if err := h.st.DeleteRuntimePolicyRule(r.Context(), rule.ID, user.ID); err != nil && err != store.ErrNotFound {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not replace tool rule")
			return
		}
	}

	// For the "allow" path, only short-circuit when no global rule
	// overrides this tool. If a global rule (AgentID nil) exists,
	// returning Source="default" would lie — the agent would still be
	// blocked. Persist an explicit agent-scoped "allow" so the
	// agent-scoped rule wins on subsequent reads.
	if action == "allow" && !hasGlobalToolRuleConflict(rules, toolName) {
		writeJSON(w, http.StatusOK, runtimeToolControlResponse{
			AgentID:  agentID,
			ToolName: toolName,
			Action:   "allow",
			Source:   "default",
		})
		return
	}

	rule := &store.RuntimePolicyRule{
		ID:         uuid.NewString(),
		UserID:     user.ID,
		AgentID:    &agentID,
		Kind:       "tool",
		Action:     action,
		ToolName:   toolName,
		InputShape: json.RawMessage(`{}`),
		Reason:     defaultToolControlReason(action, toolName),
		Source:     "user",
		Enabled:    true,
	}
	if err := h.st.CreateRuntimePolicyRule(r.Context(), rule); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create tool rule")
		return
	}
	writeJSON(w, http.StatusOK, runtimeToolControlResponse{
		AgentID:  agentID,
		ToolName: toolName,
		Action:   action,
		RuleID:   rule.ID,
		Source:   "rule",
	})
}

func readStringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s := readString(item); strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func preferToolControlSource(current, next string) string {
	rank := map[string]int{"default": 0, "request": 1, "observed": 2, "rule": 3}
	if rank[next] > rank[current] {
		return next
	}
	return current
}

func normalizeToolControlAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "allow":
		return "allow"
	case "ask", "review":
		return "review"
	case "block", "deny":
		return "deny"
	default:
		return ""
	}
}

// hasGlobalToolRuleConflict reports whether the supplied rules include a
// global (non-agent-scoped) simple tool rule for toolName whose action is
// not "allow". When true, the upsert handler must persist an explicit
// agent-scoped "allow" override rather than implying default behavior —
// otherwise the agent stays blocked by the global rule and the API lies
// to the caller about the effective action.
func hasGlobalToolRuleConflict(rules []*store.RuntimePolicyRule, toolName string) bool {
	for _, rule := range rules {
		if rule == nil || !rule.Enabled || rule.AgentID != nil {
			continue
		}
		if rule.ToolName != toolName || !isSimpleToolControlRule(rule) {
			continue
		}
		if normalizeToolControlAction(rule.Action) != "allow" {
			return true
		}
	}
	return false
}

func isSimpleToolControlRule(rule *store.RuntimePolicyRule) bool {
	if rule == nil || strings.TrimSpace(rule.InputRegex) != "" {
		return false
	}
	return rawJSONEmptyObject(rule.InputShape)
}

func rawJSONEmptyObject(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return len(obj) == 0
}

func defaultToolControlReason(action, toolName string) string {
	switch action {
	case "review":
		return "Ask before running " + toolName
	case "deny":
		return "Block " + toolName
	default:
		return ""
	}
}
