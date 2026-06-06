package llmproxy

import (
	"encoding/json"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/shellpolicy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func ReadOnlyShellCommandsAllowed(toolName, agentID string, rules []*store.RuntimePolicyRule) bool {
	return shellpolicy.ReadOnlyShellCommandsAllowed(toolName, agentID, rules)
}

func IsShellPollTool(name string, raw json.RawMessage) bool {
	return shellpolicy.IsShellPollTool(name, raw)
}

func ShellCommandFromInput(raw json.RawMessage) string {
	return shellpolicy.ShellCommandFromInput(raw)
}
