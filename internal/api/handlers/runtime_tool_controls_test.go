package handlers

import (
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// Regression: hasGlobalToolRuleConflict must consider only the NEWEST
// global rule, not any historical match. Pre-fix, a stale older deny
// would force an explicit agent-scoped allow override even after the
// global rule was updated to allow.
func TestHasGlobalToolRuleConflict_OnlyNewestGlobalRuleWins(t *testing.T) {
	// Rules arrive newest-first (created_at DESC). The newest global
	// rule says allow; older global rules in the list should be
	// ignored.
	newest := &store.RuntimePolicyRule{
		ID:        "newest",
		Kind:      "tool",
		ToolName:  "Bash",
		Action:    "allow",
		Enabled:   true,
		CreatedAt: time.Now(),
	}
	older := &store.RuntimePolicyRule{
		ID:        "older",
		Kind:      "tool",
		ToolName:  "Bash",
		Action:    "deny",
		Enabled:   true,
		CreatedAt: time.Now().Add(-time.Hour),
	}
	rules := []*store.RuntimePolicyRule{newest, older}
	if hasGlobalToolRuleConflict(rules, "Bash") {
		t.Errorf("newest=allow + older=deny: expected no conflict (older rule is stale)")
	}

	// Inverse: newest=deny should produce a conflict regardless of
	// older allows.
	newest.Action = "deny"
	older.Action = "allow"
	if !hasGlobalToolRuleConflict(rules, "Bash") {
		t.Errorf("newest=deny + older=allow: expected conflict (newest is authoritative)")
	}
}

func TestHasGlobalToolRuleConflict_IgnoresAgentScoped(t *testing.T) {
	agentID := "agent-1"
	agentScoped := &store.RuntimePolicyRule{
		ID:       "agent",
		Kind:     "tool",
		ToolName: "Bash",
		Action:   "deny",
		Enabled:  true,
		AgentID:  &agentID,
	}
	rules := []*store.RuntimePolicyRule{agentScoped}
	if hasGlobalToolRuleConflict(rules, "Bash") {
		t.Errorf("agent-scoped rule must not produce a global conflict")
	}
}

func TestHasGlobalToolRuleConflict_IgnoresDisabled(t *testing.T) {
	disabled := &store.RuntimePolicyRule{
		ID:       "disabled",
		Kind:     "tool",
		ToolName: "Bash",
		Action:   "deny",
		Enabled:  false,
	}
	rules := []*store.RuntimePolicyRule{disabled}
	if hasGlobalToolRuleConflict(rules, "Bash") {
		t.Errorf("disabled rule must not produce a conflict")
	}
}
