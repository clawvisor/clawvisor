package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/runtime/toolnames"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type runtimeToolControlResponse struct {
	AgentID                       string                     `json:"agent_id"`
	ToolName                      string                     `json:"tool_name"`
	Action                        string                     `json:"action"`
	RuleID                        string                     `json:"rule_id,omitempty"`
	Source                        string                     `json:"source"`
	Scope                         string                     `json:"scope,omitempty"`
	GlobalAction                  string                     `json:"global_action"`
	GlobalRuleID                  string                     `json:"global_rule_id,omitempty"`
	AgentAction                   string                     `json:"agent_action"`
	AgentRuleID                   string                     `json:"agent_rule_id,omitempty"`
	ReadOnlyCommandsAllowed       bool                       `json:"read_only_commands_allowed"`
	GlobalReadOnlyCommandsAllowed *bool                      `json:"global_read_only_commands_allowed,omitempty"`
	GlobalReadOnlyCommandsRuleID  string                     `json:"global_read_only_commands_rule_id,omitempty"`
	AgentReadOnlyCommandsAllowed  *bool                      `json:"agent_read_only_commands_allowed,omitempty"`
	AgentReadOnlyCommandsRuleID   string                     `json:"agent_read_only_commands_rule_id,omitempty"`
	LastSeenAt                    *time.Time                 `json:"last_seen_at,omitempty"`
	AdvancedRuleCount             int                        `json:"advanced_rule_count"`
	AdvancedRules                 []*store.RuntimePolicyRule `json:"advanced_rules"`
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
	agent, err := loadUserAgent(r.Context(), h.st, user.ID, agentID)
	if err != nil {
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
				AgentID:                 agentID,
				ToolName:                name,
				Action:                  "unset",
				Source:                  "default",
				Scope:                   "unset",
				GlobalAction:            "unset",
				AgentAction:             "unset",
				ReadOnlyCommandsAllowed: toolnames.IsShellToolName(name),
				LastSeenAt:              nil,
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
	discoveredTools := []string{}
	var latestEndpointAt *time.Time
	latestAvailableTools := []string{}
	type observedToolUse struct {
		Name      string
		Timestamp time.Time
	}
	observedTools := []observedToolUse{}
	for _, entry := range entries {
		var params map[string]any
		if len(entry.ParamsSafe) > 0 {
			_ = json.Unmarshal(entry.ParamsSafe, &params)
		}
		switch readString(params["event"]) {
		case "lite_proxy.endpoint_call":
			if latestEndpointAt == nil || entry.Timestamp.After(*latestEndpointAt) {
				ts := entry.Timestamp
				latestEndpointAt = &ts
				latestAvailableTools = nil
				for _, name := range readStringSlice(params["available_tools"]) {
					latestAvailableTools = appendToolName(latestAvailableTools, name)
				}
			}
		case "lite_proxy.tool_use_inspected":
			toolName := readString(params["tool_name"])
			if strings.TrimSpace(toolName) != "" {
				observedTools = append(observedTools, observedToolUse{Name: toolName, Timestamp: entry.Timestamp})
			}
		}
	}
	latestToolSet := toolNameSet(latestAvailableTools)
	currentShellTool := firstShellLikeToolName(latestAvailableTools)
	displayToolName := func(name string) string {
		name = strings.TrimSpace(name)
		if currentShellTool != "" && toolnames.IsShellToolName(name) && !latestToolSet[strings.ToLower(name)] {
			return currentShellTool
		}
		return name
	}
	for _, name := range latestAvailableTools {
		discoveredTools = appendToolName(discoveredTools, name)
		ctrl := ensure(name)
		if ctrl == nil {
			continue
		}
		ctrl.Source = preferToolControlSource(ctrl.Source, "request")
		if latestEndpointAt != nil && (ctrl.LastSeenAt == nil || latestEndpointAt.After(*ctrl.LastSeenAt)) {
			ts := *latestEndpointAt
			ctrl.LastSeenAt = &ts
		}
	}
	for _, observed := range observedTools {
		toolName := displayToolName(observed.Name)
		discoveredTools = appendToolName(discoveredTools, toolName)
		ctrl := ensure(toolName)
		if ctrl == nil {
			continue
		}
		ctrl.Source = preferToolControlSource(ctrl.Source, "observed")
		if ctrl.LastSeenAt == nil || observed.Timestamp.After(*ctrl.LastSeenAt) {
			ts := observed.Timestamp
			ctrl.LastSeenAt = &ts
		}
	}
	if err := ensureDefaultToolRules(r.Context(), h.st, agent, discoveredTools); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not sync default tool rules")
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
	agentScopedTools := map[string]bool{}
	for _, rule := range rules {
		if rule == nil || strings.TrimSpace(rule.ToolName) == "" {
			continue
		}
		if toolnames.IsReadOnlyShellSettingRule(rule) {
			allowed := strings.EqualFold(strings.TrimSpace(rule.Action), "allow")
			for _, ctrl := range readOnlyShellSettingControls(controls, ensure, rule.ToolName) {
				if rule.AgentID != nil {
					ctrl.AgentReadOnlyCommandsAllowed = &allowed
					ctrl.AgentReadOnlyCommandsRuleID = rule.ID
				} else {
					ctrl.GlobalReadOnlyCommandsAllowed = &allowed
					ctrl.GlobalReadOnlyCommandsRuleID = rule.ID
				}
			}
			continue
		}
		controlToolName := displayToolName(rule.ToolName)
		ctrl := ensure(controlToolName)
		if ctrl == nil {
			continue
		}
		if isSimpleToolControlRule(rule) {
			if !rule.Enabled {
				continue
			}
			action := toolControlActionForRule(rule)
			if rule.AgentID != nil {
				if ctrl.AgentRuleID == "" {
					ctrl.AgentAction = action
					ctrl.AgentRuleID = rule.ID
				}
				if !agentScopedTools[controlToolName] {
					ctrl.Action = action
					ctrl.RuleID = rule.ID
					ctrl.Source = "rule"
					ctrl.Scope = "agent"
					agentScopedTools[controlToolName] = true
				}
			} else if ctrl.GlobalRuleID == "" {
				ctrl.GlobalAction = action
				ctrl.GlobalRuleID = rule.ID
				if !agentScopedTools[controlToolName] {
					ctrl.Action = action
					ctrl.RuleID = rule.ID
					ctrl.Source = "rule"
					ctrl.Scope = "global"
				}
			}
			continue
		}
		ctrl.AdvancedRuleCount++
		ctrl.AdvancedRules = append(ctrl.AdvancedRules, rule)
		if ctrl.Scope != "agent" && rule.AgentID == nil {
			ctrl.Scope = "global"
		} else if rule.AgentID != nil {
			ctrl.Scope = "agent"
		}
	}

	out := make([]*runtimeToolControlResponse, 0, len(controls))
	for _, ctrl := range controls {
		ctrl.ReadOnlyCommandsAllowed = effectiveReadOnlyShellCommandsAllowed(ctrl)
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
		AgentID                 string `json:"agent_id"`
		ToolName                string `json:"tool_name"`
		Action                  string `json:"action"`
		Scope                   string `json:"scope"`
		ReadOnlyCommandsAllowed *bool  `json:"read_only_commands_allowed"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	agentID := strings.TrimSpace(body.AgentID)
	toolName := strings.TrimSpace(body.ToolName)
	action := normalizeToolControlAction(body.Action)
	scope := strings.ToLower(strings.TrimSpace(body.Scope))
	if scope == "" {
		scope = "agent"
	}
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "agent_id is required")
		return
	}
	if toolName == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "tool_name is required")
		return
	}
	if action == "" && body.ReadOnlyCommandsAllowed == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "action must be unset, allow, review, or deny")
		return
	}
	if scope != "agent" && scope != "global" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "scope must be agent or global")
		return
	}
	if _, err := loadUserAgent(r.Context(), h.st, user.ID, agentID); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "agent_id must belong to the current user")
		return
	}
	if body.ReadOnlyCommandsAllowed != nil {
		if !toolnames.IsShellToolName(toolName) {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "read-only command setting only applies to shell-like tools")
			return
		}
		if err := h.upsertReadOnlyShellSetting(r.Context(), user.ID, agentID, toolName, scope, *body.ReadOnlyCommandsAllowed); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update read-only shell setting")
			return
		}
		if action == "" {
			resp := runtimeToolControlResponse{
				AgentID:                 agentID,
				ToolName:                toolName,
				Action:                  "unset",
				Source:                  "default",
				Scope:                   "unset",
				GlobalAction:            "unset",
				AgentAction:             "unset",
				ReadOnlyCommandsAllowed: *body.ReadOnlyCommandsAllowed,
			}
			if scope == "global" {
				resp.GlobalReadOnlyCommandsAllowed = body.ReadOnlyCommandsAllowed
			} else {
				resp.AgentReadOnlyCommandsAllowed = body.ReadOnlyCommandsAllowed
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}

	ruleAgentID := &agentID
	ruleFilterAgentID := agentID
	if scope == "global" {
		ruleAgentID = nil
		ruleFilterAgentID = ""
	}
	rules, err := h.st.ListRuntimePolicyRules(r.Context(), user.ID, store.RuntimePolicyRuleFilter{
		AgentID: ruleFilterAgentID,
		Kind:    "tool",
		Limit:   500,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list tool rules")
		return
	}
	for _, rule := range rules {
		if rule == nil || !toolRuleNamesSameControl(rule.ToolName, toolName) || !isSimpleToolControlRule(rule) {
			continue
		}
		if scope == "agent" && (rule.AgentID == nil || *rule.AgentID != agentID) {
			continue
		}
		if scope == "global" && rule.AgentID != nil {
			continue
		}
		if err := h.st.DeleteRuntimePolicyRule(r.Context(), rule.ID, user.ID); err != nil && err != store.ErrNotFound {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not replace tool rule")
			return
		}
	}

	if action == "unset" {
		if scope == "global" {
			writeJSON(w, http.StatusOK, runtimeToolControlResponse{
				AgentID:      agentID,
				ToolName:     toolName,
				Action:       "unset",
				Source:       "default",
				Scope:        "unset",
				GlobalAction: "unset",
				AgentAction:  "unset",
			})
			return
		}
		rule := &store.RuntimePolicyRule{
			ID:         uuid.NewString(),
			UserID:     user.ID,
			AgentID:    ruleAgentID,
			Kind:       "tool",
			Action:     "review",
			ToolName:   toolName,
			InputShape: json.RawMessage(`{}`),
			Reason:     "Use task scopes for " + toolName,
			Source:     "user",
			Enabled:    true,
		}
		if err := h.st.CreateRuntimePolicyRule(r.Context(), rule); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create tool rule")
			return
		}
		writeJSON(w, http.StatusOK, runtimeToolControlResponse{
			AgentID:      agentID,
			ToolName:     toolName,
			Action:       "unset",
			Source:       "default",
			Scope:        "unset",
			GlobalAction: "unset",
			AgentAction:  "unset",
			AgentRuleID:  rule.ID,
		})
		return
	}

	rule := &store.RuntimePolicyRule{
		ID:         uuid.NewString(),
		UserID:     user.ID,
		AgentID:    ruleAgentID,
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
	resp := runtimeToolControlResponse{
		AgentID:      agentID,
		ToolName:     toolName,
		Action:       action,
		RuleID:       rule.ID,
		Source:       "rule",
		Scope:        scope,
		GlobalAction: "unset",
		AgentAction:  "unset",
	}
	if scope == "global" {
		resp.GlobalAction = action
		resp.GlobalRuleID = rule.ID
	} else {
		resp.AgentAction = action
		resp.AgentRuleID = rule.ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func readOnlyShellSettingControls(controls map[string]*runtimeToolControlResponse, ensure func(string) *runtimeToolControlResponse, toolName string) []*runtimeToolControlResponse {
	if !toolnames.IsShellToolName(toolName) {
		if ctrl := ensure(toolName); ctrl != nil {
			return []*runtimeToolControlResponse{ctrl}
		}
		return nil
	}
	out := make([]*runtimeToolControlResponse, 0, 1)
	for _, ctrl := range controls {
		if ctrl != nil && toolnames.IsShellToolName(ctrl.ToolName) {
			out = append(out, ctrl)
		}
	}
	if len(out) > 0 {
		sort.Slice(out, func(i, j int) bool {
			return strings.ToLower(out[i].ToolName) < strings.ToLower(out[j].ToolName)
		})
		return out
	}
	if ctrl := ensure(toolName); ctrl != nil {
		return []*runtimeToolControlResponse{ctrl}
	}
	return nil
}

func toolNameSet(tools []string) map[string]bool {
	set := make(map[string]bool, len(tools))
	for _, tool := range tools {
		tool = strings.TrimSpace(tool)
		if tool != "" {
			set[strings.ToLower(tool)] = true
		}
	}
	return set
}

func firstShellLikeToolName(tools []string) string {
	for _, tool := range tools {
		tool = strings.TrimSpace(tool)
		if toolnames.IsShellToolName(tool) {
			return tool
		}
	}
	return ""
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
	case "unset", "default":
		return "unset"
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

func toolControlActionForRule(rule *store.RuntimePolicyRule) string {
	action := normalizeToolControlAction(rule.Action)
	if action == "review" && rule.Source == "user" && strings.HasPrefix(strings.TrimSpace(rule.Reason), "Use task scopes for ") {
		return "unset"
	}
	return action
}

func isSimpleToolControlRule(rule *store.RuntimePolicyRule) bool {
	if rule == nil || strings.TrimSpace(rule.InputRegex) != "" {
		return false
	}
	if toolnames.IsReadOnlyShellSettingRule(rule) {
		return false
	}
	return rawJSONEmptyObject(rule.InputShape)
}

func effectiveReadOnlyShellCommandsAllowed(ctrl *runtimeToolControlResponse) bool {
	if ctrl == nil || !toolnames.IsShellToolName(ctrl.ToolName) {
		return false
	}
	allowed := true
	if ctrl.GlobalReadOnlyCommandsAllowed != nil {
		allowed = *ctrl.GlobalReadOnlyCommandsAllowed
	}
	if ctrl.AgentReadOnlyCommandsAllowed != nil {
		allowed = *ctrl.AgentReadOnlyCommandsAllowed
	}
	return allowed
}

func toolRuleNamesSameControl(ruleToolName, controlToolName string) bool {
	if toolnames.IsShellToolName(ruleToolName) && toolnames.IsShellToolName(controlToolName) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(ruleToolName), strings.TrimSpace(controlToolName))
}

func (h *RuntimeHandler) upsertReadOnlyShellSetting(ctx context.Context, userID, agentID, toolName, scope string, allowed bool) error {
	ruleAgentID := &agentID
	filterAgentID := agentID
	if scope == "global" {
		ruleAgentID = nil
		filterAgentID = ""
	}
	rules, err := h.st.ListRuntimePolicyRules(ctx, userID, store.RuntimePolicyRuleFilter{
		AgentID: filterAgentID,
		Kind:    "tool",
		Limit:   500,
	})
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if rule == nil || !toolnames.IsReadOnlyShellSettingRule(rule) || !toolnames.ToolNamesSameClass(rule.ToolName, toolName) {
			continue
		}
		if scope == "agent" && (rule.AgentID == nil || *rule.AgentID != agentID) {
			continue
		}
		if scope == "global" && rule.AgentID != nil {
			continue
		}
		if err := h.st.DeleteRuntimePolicyRule(ctx, rule.ID, userID); err != nil && err != store.ErrNotFound {
			return err
		}
	}
	action := "deny"
	reason := "Require task scope or approval for read-only shell commands"
	if allowed {
		action = "allow"
		reason = "Allow read-only shell commands without task scope"
	}
	return h.st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:         uuid.NewString(),
		UserID:     userID,
		AgentID:    ruleAgentID,
		Kind:       "tool",
		Action:     action,
		ToolName:   toolName,
		InputShape: toolnames.ReadOnlyShellSettingInputShape(),
		Reason:     reason,
		Source:     toolnames.ReadOnlyShellSettingSource,
		Enabled:    true,
	})
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
		return "Review before running " + toolName
	case "deny":
		return "Always deny " + toolName
	case "allow":
		return "Always allow " + toolName
	default:
		return ""
	}
}
