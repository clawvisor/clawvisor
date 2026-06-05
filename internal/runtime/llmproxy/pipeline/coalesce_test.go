package pipeline_test

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// TestCoalesceHolds_GroupsBySharedKey verifies the core coalescing
// invariant: tool_uses with matching HoldKey collapse into one group.
func TestCoalesceHolds_GroupsBySharedKey(t *testing.T) {
	r := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_a": {Outcome: pipeline.OutcomeHold, HoldKey: "k1"},
			"toolu_b": {Outcome: pipeline.OutcomeHold, HoldKey: "k1"},
			"toolu_c": {Outcome: pipeline.OutcomeHold, HoldKey: "k2"},
			"toolu_d": {Outcome: pipeline.OutcomeAllow},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{ToolUseID: "toolu_a"},
			{ToolUseID: "toolu_b"},
			{ToolUseID: "toolu_c"},
			{ToolUseID: "toolu_d"},
		},
	}

	groups := pipeline.CoalesceHolds(r)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// Group ordering follows first-seen order of HoldKeys in
	// Evaluations.
	if groups[0].HoldKey != "k1" {
		t.Errorf("first group HoldKey = %q, want k1", groups[0].HoldKey)
	}
	if len(groups[0].ToolUseIDs) != 2 {
		t.Errorf("k1 should group 2 tool_uses, got %d (%v)", len(groups[0].ToolUseIDs), groups[0].ToolUseIDs)
	}
	if groups[0].ToolUseIDs[0] != "toolu_a" || groups[0].ToolUseIDs[1] != "toolu_b" {
		t.Errorf("k1 tool_use order wrong: %v", groups[0].ToolUseIDs)
	}

	if groups[1].HoldKey != "k2" {
		t.Errorf("second group HoldKey = %q, want k2", groups[1].HoldKey)
	}
	if len(groups[1].ToolUseIDs) != 1 || groups[1].ToolUseIDs[0] != "toolu_c" {
		t.Errorf("k2 group wrong: %v", groups[1].ToolUseIDs)
	}
}

// TestCoalesceHolds_IgnoresNonHoldVerdicts verifies non-Hold verdicts
// don't appear in any group.
func TestCoalesceHolds_IgnoresNonHoldVerdicts(t *testing.T) {
	r := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_a": {Outcome: pipeline.OutcomeAllow},
			"toolu_b": {Outcome: pipeline.OutcomeDeny},
			"toolu_c": {Outcome: pipeline.OutcomeRewrite, HoldKey: "k1"},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{ToolUseID: "toolu_a"},
			{ToolUseID: "toolu_b"},
			{ToolUseID: "toolu_c"},
		},
	}

	groups := pipeline.CoalesceHolds(r)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups (no Hold verdicts), got %d", len(groups))
	}
}

// TestCoalesceHolds_IgnoresHoldsWithoutKey verifies a Hold without a
// HoldKey doesn't get coalesced.
func TestCoalesceHolds_IgnoresHoldsWithoutKey(t *testing.T) {
	r := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_a": {Outcome: pipeline.OutcomeHold}, // no HoldKey
			"toolu_b": {Outcome: pipeline.OutcomeHold, HoldKey: "k1"},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{ToolUseID: "toolu_a"},
			{ToolUseID: "toolu_b"},
		},
	}

	groups := pipeline.CoalesceHolds(r)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group (k1 only), got %d", len(groups))
	}
	if groups[0].HoldKey != "k1" || groups[0].ToolUseIDs[0] != "toolu_b" {
		t.Errorf("unexpected group: %+v", groups[0])
	}
}

// TestCoalesceHolds_DeduplicatesPerToolUse verifies that a tool_use
// appearing in multiple Evaluations (because it ran through multiple
// evaluators) only appears once in the coalesced output.
func TestCoalesceHolds_DeduplicatesPerToolUse(t *testing.T) {
	r := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_a": {Outcome: pipeline.OutcomeHold, HoldKey: "k1"},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{ToolUseID: "toolu_a", EvaluatorName: "first"},
			{ToolUseID: "toolu_a", EvaluatorName: "second"},
			{ToolUseID: "toolu_a", EvaluatorName: "third"},
		},
	}

	groups := pipeline.CoalesceHolds(r)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0].ToolUseIDs) != 1 {
		t.Errorf("tool_use appeared multiple times in coalesced output: %v", groups[0].ToolUseIDs)
	}
}

// TestCoalesceHolds_NilInput pins the nil safety.
func TestCoalesceHolds_NilInput(t *testing.T) {
	if groups := pipeline.CoalesceHolds(nil); groups != nil {
		t.Errorf("nil input should return nil, got %v", groups)
	}
}
