package policy

import (
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestMatchRuntimePolicyToolTreatsSystemDefaultsAsFallback(t *testing.T) {
	agentID := "agent-1"
	rules := []*store.RuntimePolicyRule{
		{
			ID:         "agent-system-allow",
			AgentID:    &agentID,
			Kind:       "tool",
			Action:     "allow",
			ToolName:   "Read",
			InputShape: json.RawMessage(`{}`),
			Source:     "system",
			Enabled:    true,
		},
		{
			ID:         "global-user-deny",
			Kind:       "tool",
			Action:     "deny",
			ToolName:   "Read",
			InputShape: json.RawMessage(`{}`),
			Source:     "user",
			Enabled:    true,
		},
	}
	got, err := MatchRuntimePolicyTool(rules, agentID, "Read", map[string]any{})
	if err != nil {
		t.Fatalf("MatchRuntimePolicyTool: %v", err)
	}
	if got == nil || got.ID != "global-user-deny" {
		t.Fatalf("user global deny should outrank agent-scoped system default, got %+v", got)
	}

	rules = append(rules, &store.RuntimePolicyRule{
		ID:         "agent-user-allow",
		AgentID:    &agentID,
		Kind:       "tool",
		Action:     "allow",
		ToolName:   "Read",
		InputShape: json.RawMessage(`{}`),
		Source:     "user",
		Enabled:    true,
	})
	got, err = MatchRuntimePolicyTool(rules, agentID, "Read", map[string]any{})
	if err != nil {
		t.Fatalf("MatchRuntimePolicyTool with user agent allow: %v", err)
	}
	if got == nil || got.ID != "agent-user-allow" {
		t.Fatalf("agent-scoped user allow should outrank global user deny, got %+v", got)
	}
}

func TestMatchRuntimePolicyToolIgnoresReadOnlyShellSettingMarker(t *testing.T) {
	agentID := "agent-1"
	rules := []*store.RuntimePolicyRule{{
		ID:         "readonly-shell-marker",
		AgentID:    &agentID,
		Kind:       "tool",
		Action:     "allow",
		ToolName:   "Bash",
		InputShape: json.RawMessage(`{"clawvisor_readonly_shell_setting":true}`),
		Source:     "readonly_shell_setting",
		Enabled:    true,
	}}
	got, err := MatchRuntimePolicyTool(rules, agentID, "Bash", map[string]any{"clawvisor_readonly_shell_setting": true})
	if err != nil {
		t.Fatalf("MatchRuntimePolicyTool: %v", err)
	}
	if got != nil {
		t.Fatalf("read-only shell marker must not act as a normal tool allow rule, got %+v", got)
	}
}
