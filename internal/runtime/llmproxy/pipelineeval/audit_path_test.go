package pipelineeval

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
)

// TestClassifyTaskScopePath_FromFacts pins how the audit row's
// `task_scope_path` enum is derived from the chain's AuthorizationFact.
// This is the field operators query to verify per-conversation
// isolation: every preferred_strict is a checked-out task that
// covered the call; every preferred_mismatch_blocked is the new
// block class that would have been a cross-conversation leak before
// this fix.
func TestClassifyTaskScopePath_FromFacts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		factOutcome string
		preferred   string
		want        string
	}{
		{
			name:        "preferred set, scope allow -> preferred_strict",
			factOutcome: string(runtimedecision.SourceTaskScope),
			preferred:   "task-1",
			want:        "preferred_strict",
		},
		{
			name:        "no preferred, scope allow -> no_preferred_fallback",
			factOutcome: string(runtimedecision.SourceTaskScope),
			preferred:   "",
			want:        "no_preferred_fallback",
		},
		{
			name:        "preferred set, mismatch source -> preferred_mismatch_blocked",
			factOutcome: string(runtimedecision.SourceTaskScopeMismatchPreferred),
			preferred:   "task-1",
			want:        "preferred_mismatch_blocked",
		},
		{
			name:        "rule allow source -> empty (not a scope decision)",
			factOutcome: string(runtimedecision.SourceRuleAllow),
			preferred:   "task-1",
			want:        "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := []conversation.EvaluationFact{
				conversation.AuthorizationFact{Outcome: tc.factOutcome},
			}
			if got := classifyTaskScopePath(facts, tc.preferred); got != tc.want {
				t.Errorf("classifyTaskScopePath outcome=%q preferred=%q = %q, want %q",
					tc.factOutcome, tc.preferred, got, tc.want)
			}
		})
	}
}

// TestClassifyTaskScopePathFromSource mirrors the from-facts cases
// against the inline-string variant used by the legacy resolver.
func TestClassifyTaskScopePathFromSource(t *testing.T) {
	t.Parallel()
	if got := classifyTaskScopePathFromSource(string(runtimedecision.SourceTaskScope), "task-1"); got != "preferred_strict" {
		t.Errorf("preferred + scope allow: got %q, want preferred_strict", got)
	}
	if got := classifyTaskScopePathFromSource(string(runtimedecision.SourceTaskScope), ""); got != "no_preferred_fallback" {
		t.Errorf("no preferred + scope allow: got %q, want no_preferred_fallback", got)
	}
	if got := classifyTaskScopePathFromSource(string(runtimedecision.SourceTaskScopeMismatchPreferred), "task-1"); got != "preferred_mismatch_blocked" {
		t.Errorf("mismatch: got %q, want preferred_mismatch_blocked", got)
	}
	if got := classifyTaskScopePathFromSource(string(runtimedecision.SourceRuleAllow), "task-1"); got != "" {
		t.Errorf("rule allow: got %q, want empty (not a scope decision)", got)
	}
}
