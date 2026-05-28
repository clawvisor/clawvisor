package toolnames

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestIsSensitiveFileGuardableTool(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"Bash", true},
		{"shell", true},
		{"Read", true},
		{"read_file", true},
		{"Glob", true},
		{"Grep", true},
		{"LS", true},
		{"WebFetch", false},
		{"Write", false},
		{"unknown_tool", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSensitiveFileGuardableTool(tc.name); got != tc.want {
				t.Fatalf("IsSensitiveFileGuardableTool(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestSensitiveFileGuardEnabledDefaultsOn(t *testing.T) {
	if !SensitiveFileGuardEnabled("Read", "agent-1", nil) {
		t.Fatal("expected guard to default ON when no marker rule")
	}
}

func TestSensitiveFileGuardEnabledRespectsGlobalOff(t *testing.T) {
	rules := []*store.RuntimePolicyRule{
		guardMarkerRule(t, "Read", nil, "allow"),
	}
	if SensitiveFileGuardEnabled("Read", "agent-1", rules) {
		t.Fatal("expected global allow marker to disable guard")
	}
}

func TestSensitiveFileGuardEnabledAgentOverrideWinsOverGlobal(t *testing.T) {
	agentID := "agent-1"
	rules := []*store.RuntimePolicyRule{
		guardMarkerRule(t, "Read", nil, "allow"),     // global: off
		guardMarkerRule(t, "Read", &agentID, "deny"), // agent: on
	}
	if !SensitiveFileGuardEnabled("Read", agentID, rules) {
		t.Fatal("expected agent-scoped deny to win over global allow")
	}
}

func TestSensitiveFileGuardEnabledMatchesByClass(t *testing.T) {
	// A rule keyed to "Bash" should apply to any shell-class tool.
	rules := []*store.RuntimePolicyRule{
		guardMarkerRule(t, "Bash", nil, "allow"),
	}
	if SensitiveFileGuardEnabled("shell", "agent-1", rules) {
		t.Fatal("expected shell tool to inherit Bash marker disable")
	}
}

func TestSensitiveFileGuardEnabledReturnsFalseForNonGuardableTool(t *testing.T) {
	if SensitiveFileGuardEnabled("WebFetch", "agent-1", nil) {
		t.Fatal("non-guardable tool should not be gated")
	}
}

func guardMarkerRule(t *testing.T, toolName string, agentID *string, action string) *store.RuntimePolicyRule {
	t.Helper()
	return &store.RuntimePolicyRule{
		Kind:       "tool",
		Action:     action,
		ToolName:   toolName,
		AgentID:    agentID,
		Source:     SensitiveFileGuardSettingSource,
		InputShape: SensitiveFileGuardSettingInputShape(),
		Enabled:    true,
	}
}
