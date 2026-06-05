package pipeline_test

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// TestShouldCoalesce_RequiresMultipleToolUses pins the cardinality gate.
func TestShouldCoalesce_RequiresMultipleToolUses(t *testing.T) {
	r := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_a": {Outcome: pipeline.OutcomeHold, HoldKey: "k1"},
		},
	}
	if pipeline.ShouldCoalesce(r) {
		t.Errorf("single-tool result should not coalesce")
	}
}

// TestShouldCoalesce_RequiresAtLeastOneHold pins the "no Holds → no
// coalescing" rule.
func TestShouldCoalesce_RequiresAtLeastOneHold(t *testing.T) {
	r := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_a": {Outcome: pipeline.OutcomeAllow},
			"toolu_b": {Outcome: pipeline.OutcomeAllow},
			"toolu_c": {Outcome: pipeline.OutcomeRewrite},
		},
	}
	if pipeline.ShouldCoalesce(r) {
		t.Errorf("no Holds present → should not coalesce")
	}
}

// TestShouldCoalesce_DenyBreaksCoalescing pins the hard-deny rule:
// mixing approve + permanently-blocked in one prompt is confusing UX.
func TestShouldCoalesce_DenyBreaksCoalescing(t *testing.T) {
	r := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_a": {Outcome: pipeline.OutcomeHold, HoldKey: "k1"},
			"toolu_b": {Outcome: pipeline.OutcomeDeny},
		},
	}
	if pipeline.ShouldCoalesce(r) {
		t.Errorf("hard Deny in turn should break coalescing")
	}
}

// TestShouldCoalesce_InlineTaskHoldBreaksCoalescing pins that
// inline-task holds (HoldKey prefix "inline_task_") don't coalesce
// with sibling approvals.
func TestShouldCoalesce_InlineTaskHoldBreaksCoalescing(t *testing.T) {
	r := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_a": {Outcome: pipeline.OutcomeHold, HoldKey: "inline_task_xyz"},
			"toolu_b": {Outcome: pipeline.OutcomeHold, HoldKey: "k1"},
		},
	}
	if pipeline.ShouldCoalesce(r) {
		t.Errorf("inline-task hold present → should not coalesce")
	}
}

// TestShouldCoalesce_AllowedHappyPath pins the actual coalescing
// trigger: multiple tool_uses + at least one standard Hold + no Deny +
// no inline-task hold.
func TestShouldCoalesce_AllowedHappyPath(t *testing.T) {
	r := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_a": {Outcome: pipeline.OutcomeHold, HoldKey: "k1"},
			"toolu_b": {Outcome: pipeline.OutcomeHold, HoldKey: "k1"},
			"toolu_c": {Outcome: pipeline.OutcomeAllow},
		},
	}
	if !pipeline.ShouldCoalesce(r) {
		t.Errorf("multi-tool + Hold + no Deny + no inline-task → should coalesce")
	}
}

// TestShouldCoalesce_NilSafe pins nil input handling.
func TestShouldCoalesce_NilSafe(t *testing.T) {
	if pipeline.ShouldCoalesce(nil) {
		t.Errorf("nil result → should not coalesce")
	}
}
