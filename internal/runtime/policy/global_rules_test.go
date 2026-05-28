package policy

import (
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/runtime/toolnames"
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
		InputShape: toolnames.ReadOnlyShellSettingInputShape(),
		Source:     toolnames.ReadOnlyShellSettingSource,
		Enabled:    true,
	}}
	got, err := MatchRuntimePolicyTool(rules, agentID, "Bash", map[string]any{toolnames.ReadOnlyShellSettingShapeKey: true})
	if err != nil {
		t.Fatalf("MatchRuntimePolicyTool: %v", err)
	}
	if got != nil {
		t.Fatalf("read-only shell marker must not act as a normal tool allow rule, got %+v", got)
	}
}

func TestMatchRuntimePolicyToolIgnoresSensitiveFileGuardMarker(t *testing.T) {
	agentID := "agent-1"
	// Cover both the enabled (deny) and disabled (allow) marker actions:
	// neither must surface as a normal authorization decision for any
	// same-class tool call.
	for _, action := range []string{"deny", "allow"} {
		t.Run(action, func(t *testing.T) {
			rules := []*store.RuntimePolicyRule{{
				ID:         "sensitive-file-guard-marker",
				AgentID:    &agentID,
				Kind:       "tool",
				Action:     action,
				ToolName:   "Bash",
				InputShape: toolnames.SensitiveFileGuardSettingInputShape(),
				Source:     toolnames.SensitiveFileGuardSettingSource,
				Enabled:    true,
			}}
			// A normal Bash call (no marker key) must not be matched.
			got, err := MatchRuntimePolicyTool(rules, agentID, "Bash", map[string]any{"command": "ls"})
			if err != nil {
				t.Fatalf("MatchRuntimePolicyTool: %v", err)
			}
			if got != nil {
				t.Fatalf("sensitive-file-guard marker must not authorize ordinary tool calls, got %+v", got)
			}
			// And a probe carrying the marker key itself must also be ignored.
			got, err = MatchRuntimePolicyTool(rules, agentID, "Bash", map[string]any{toolnames.SensitiveFileGuardSettingShapeKey: true})
			if err != nil {
				t.Fatalf("MatchRuntimePolicyTool: %v", err)
			}
			if got != nil {
				t.Fatalf("sensitive-file-guard marker must not act as a normal tool rule, got %+v", got)
			}
		})
	}
}
