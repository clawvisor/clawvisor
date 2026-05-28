package toolnames

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
)

const (
	ShellClass                        = "shell"
	ReadOnlyShellSettingSource        = "readonly_shell_setting"
	ReadOnlyShellSettingShapeKey      = "clawvisor_readonly_shell_setting"
	SensitiveFileGuardSettingSource   = "sensitive_file_guard_setting"
	SensitiveFileGuardSettingShapeKey = "clawvisor_sensitive_file_guard_setting"
)

func IsShellToolName(name string) bool {
	return ToolClass(name) == ShellClass
}

func ToolNamesSameClass(a, b string) bool {
	if strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) {
		return true
	}
	ac := ToolClass(a)
	bc := ToolClass(b)
	return ac != "" && ac == bc
}

func ToolClass(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bash", "shell", "exec", "exec_command", "mcp__shell__exec", "terminal":
		return ShellClass
	case "read", "read_file":
		return "read_file"
	case "edit", "notebookedit", "apply_patch", "edit_file":
		return "edit_file"
	case "write", "write_file":
		return "write_file"
	case "webfetch", "fetch", "http_request", "web_fetch":
		return "web_fetch"
	default:
		return ""
	}
}

func IsReadOnlyShellSettingRule(rule *store.RuntimePolicyRule) bool {
	return rule != nil &&
		rule.Kind == "tool" &&
		strings.EqualFold(strings.TrimSpace(rule.Source), ReadOnlyShellSettingSource)
}

func ReadOnlyShellSettingInputShape() json.RawMessage {
	return json.RawMessage(`{"` + ReadOnlyShellSettingShapeKey + `":true}`)
}

// IsSensitiveFileGuardSettingRule reports whether a rule is the
// marker for the sensitive-file-guard toggle (see ListToolControls).
func IsSensitiveFileGuardSettingRule(rule *store.RuntimePolicyRule) bool {
	return rule != nil &&
		rule.Kind == "tool" &&
		strings.EqualFold(strings.TrimSpace(rule.Source), SensitiveFileGuardSettingSource)
}

// SensitiveFileGuardSettingInputShape is the shape used to fingerprint
// the marker rule so it never matches normal authorization checks.
func SensitiveFileGuardSettingInputShape() json.RawMessage {
	return json.RawMessage(`{"` + SensitiveFileGuardSettingShapeKey + `":true}`)
}

// IsSensitiveFileGuardableTool reports whether the sensitive-file-guard
// toggle is meaningful for this tool. The guard fires on file reads
// (Read/Glob/Grep/LS variants) and on shell commands that touch
// sensitive paths.
func IsSensitiveFileGuardableTool(name string) bool {
	if IsShellToolName(name) {
		return true
	}
	switch ToolClass(name) {
	case "read_file":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "glob", "grep", "ls", "mcp__filesystem__read_file":
		return true
	}
	return false
}

// SensitiveFileGuardEnabled resolves whether the guard is on for a
// given tool + agent, given the current rule set. Default ON when no
// marker rule is present. Agent-scoped overrides win over global.
//
// Marker rule action convention:
//
//	allow  → guard DISABLED (file reads ride existing auto-allow paths)
//	deny   → guard ENABLED  (sensitive paths fall through to approval)
func SensitiveFileGuardEnabled(toolName, agentID string, rules []*store.RuntimePolicyRule) bool {
	if !IsSensitiveFileGuardableTool(toolName) {
		return false
	}
	global := true
	var agentOverride *bool
	for _, rule := range rules {
		if rule == nil || !rule.Enabled || !IsSensitiveFileGuardSettingRule(rule) {
			continue
		}
		if !ToolNamesSameClass(rule.ToolName, toolName) {
			continue
		}
		enabled := !strings.EqualFold(strings.TrimSpace(rule.Action), "allow")
		if rule.AgentID != nil {
			if strings.TrimSpace(*rule.AgentID) == strings.TrimSpace(agentID) {
				v := enabled
				agentOverride = &v
			}
			continue
		}
		global = enabled
	}
	if agentOverride != nil {
		return *agentOverride
	}
	return global
}
