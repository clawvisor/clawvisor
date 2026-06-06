package llmproxy

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/runtime/toolnames"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Helpers used by EvaluateTriggerMissAuthorization to classify shell
// tool_uses (readonly bypass, background-shell poll, sensitive-path
// detection) before delegating to the decision engine. Carved out of
// postprocess.go so the file stays small.

func ReadOnlyShellCommandsAllowed(toolName, agentID string, rules []*store.RuntimePolicyRule) bool {
	global := true
	agent := (*bool)(nil)
	for _, rule := range rules {
		if rule == nil || !rule.Enabled || !toolnames.IsReadOnlyShellSettingRule(rule) || !toolnames.ToolNamesSameClass(rule.ToolName, toolName) {
			continue
		}
		allowed := strings.EqualFold(strings.TrimSpace(rule.Action), "allow")
		if rule.AgentID != nil {
			if strings.TrimSpace(*rule.AgentID) == strings.TrimSpace(agentID) {
				v := allowed
				agent = &v
			}
			continue
		}
		global = allowed
	}
	if agent != nil {
		return *agent
	}
	return global
}

// IsShellPollTool reports whether a tool_use is a harness poll on a
// background shell — read-equivalent and worth passing through. The
// canonical case is Codex's `write_stdin` with empty `chars`, which
// the harness emits continuously while a backgrounded `exec_command`
// is running. Non-empty `chars` is actual input typed into a shell
// (potentially mutating); stay strict.
func IsShellPollTool(name string, raw json.RawMessage) bool {
	if name != "write_stdin" {
		return false
	}
	if len(raw) == 0 {
		return false
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return false
	}
	chars, ok := input["chars"].(string)
	if !ok {
		return false
	}
	return strings.TrimSpace(chars) == ""
}

// ShellCommandFromInput extracts the command string from a shell-tool
// input JSON. Claude Code's Bash uses `command`; Codex's exec_command
// uses `cmd`. Returns "" when neither is present or non-string.
func ShellCommandFromInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return ""
	}
	if v, ok := input["cmd"].(string); ok && v != "" {
		return v
	}
	if v, ok := input["command"].(string); ok {
		return v
	}
	return ""
}
