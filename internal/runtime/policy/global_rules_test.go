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
