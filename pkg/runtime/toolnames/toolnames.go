package toolnames

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
)

const (
	ShellClass                   = "shell"
	ReadOnlyShellSettingSource   = "readonly_shell_setting"
	ReadOnlyShellSettingShapeKey = "clawvisor_readonly_shell_setting"
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
