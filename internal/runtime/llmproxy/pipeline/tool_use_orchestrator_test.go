package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// recordingToolUseMutator captures rewrite intent for assertions.
type recordingToolUseMutator struct {
	id           string
	rewriteCalls []json.RawMessage
	replaceCalls []string
}

func (m *recordingToolUseMutator) RewriteArgs(in json.RawMessage) error {
	m.rewriteCalls = append(m.rewriteCalls, append(json.RawMessage(nil), in...))
	return nil
}
func (m *recordingToolUseMutator) ReplaceWithText(text string) error {
	m.replaceCalls = append(m.replaceCalls, text)
	return nil
}

// allowEvaluator returns Allow for every tool_use with a tag in Audit.
type allowEvaluator struct {
	name string
	tag  string
}

func (e *allowEvaluator) Name() string { return e.name }
func (e *allowEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, _ conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{
		Outcome:     pipeline.OutcomeAllow,
		AuditFields: map[string]any{"evaluator": e.tag},
	}, nil
}

// skipEvaluator returns Skip so later evaluators get a turn.
type skipEvaluator struct{ name string }

func (e *skipEvaluator) Name() string { return e.name }
func (e *skipEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, _ conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
}

// holdEvaluator returns Hold with a HoldKey.
type holdEvaluator struct {
	name    string
	holdKey string
}

func (e *holdEvaluator) Name() string { return e.name }
func (e *holdEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, _ conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeHold, HoldKey: e.holdKey}, nil
}

// continueEvaluator returns Continue, short-circuiting the whole pass.
type continueEvaluator struct{ name string }

func (e *continueEvaluator) Name() string { return e.name }
func (e *continueEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeAllow,
		Continue: &pipeline.ContinueSignal{
			PrependNotice: "continuing from " + tu.ID,
		},
	}, nil
}

// erroringEvaluator returns a Go error.
type erroringEvaluator struct{ name string }

func (e *erroringEvaluator) Name() string { return e.name }
func (e *erroringEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, _ conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{}, errors.New("explode")
}

// TestEvaluateToolUses_FirstNonSkipWins pins the "first evaluator that
// claims a tool_use stops the chain for THAT tool_use" semantic.
func TestEvaluateToolUses_FirstNonSkipWins(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{
		{ID: "toolu_1", Name: "Bash", Input: json.RawMessage(`{}`)},
		{ID: "toolu_2", Name: "Bash", Input: json.RawMessage(`{}`)},
	}

	evaluators := []pipeline.ToolUseEvaluator{
		&skipEvaluator{name: "first_skip"},
		&allowEvaluator{name: "claimer", tag: "claimed"},
		&allowEvaluator{name: "later", tag: "should_not_see"},
	}

	mutators := map[string]*recordingToolUseMutator{}
	result, err := pipeline.EvaluateToolUses(context.Background(), res, tools, evaluators, func(id string) pipeline.ToolUseMutator {
		m := &recordingToolUseMutator{id: id}
		mutators[id] = m
		return m
	})
	if err != nil {
		t.Fatalf("EvaluateToolUses: %v", err)
	}

	for _, tu := range tools {
		v := result.PerToolUse[tu.ID]
		if v.Outcome != pipeline.OutcomeAllow {
			t.Errorf("%s: Outcome = %q, want Allow", tu.ID, v.Outcome)
		}
		if v.AuditFields["evaluator"] != "claimed" {
			t.Errorf("%s: evaluator tag = %v, want claimed", tu.ID, v.AuditFields["evaluator"])
		}
	}

	// Per-tool-use × per-evaluator trail: first_skip + claimer for each
	// tool_use (later doesn't run). 2 × 2 = 4.
	if len(result.Evaluations) != 4 {
		t.Errorf("expected 4 evaluations in trail, got %d", len(result.Evaluations))
	}
}

// TestEvaluateToolUses_ContinueShortCircuits pins continuation
// semantics: a Continue signal halts the whole pass.
func TestEvaluateToolUses_ContinueShortCircuits(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{
		{ID: "toolu_local", Name: "Bash"},
		{ID: "toolu_should_not_run", Name: "Bash"},
	}

	evaluators := []pipeline.ToolUseEvaluator{
		&continueEvaluator{name: "continuer"},
	}

	result, err := pipeline.EvaluateToolUses(context.Background(), res, tools, evaluators, func(id string) pipeline.ToolUseMutator {
		return &recordingToolUseMutator{id: id}
	})
	if err != nil {
		t.Fatalf("EvaluateToolUses: %v", err)
	}

	if result.Continue == nil {
		t.Fatalf("expected Continue set")
	}
	if result.ContinueFromToolUseID != "toolu_local" {
		t.Errorf("ContinueFromToolUseID = %q, want toolu_local", result.ContinueFromToolUseID)
	}
	if _, ok := result.PerToolUse["toolu_should_not_run"]; ok {
		t.Errorf("tool_use after Continue was evaluated; should have been skipped")
	}
}

// TestEvaluateToolUses_HoldVerdictsPreservedPerTool pins that
// per-tool-use Hold verdicts collect without coalescing (Phase 5 will
// add coalescing on top).
func TestEvaluateToolUses_HoldVerdictsPreservedPerTool(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{
		{ID: "toolu_a"},
		{ID: "toolu_b"},
	}

	evaluators := []pipeline.ToolUseEvaluator{
		&holdEvaluator{name: "holder", holdKey: "shared-key"},
	}

	result, err := pipeline.EvaluateToolUses(context.Background(), res, tools, evaluators, func(id string) pipeline.ToolUseMutator {
		return &recordingToolUseMutator{id: id}
	})
	if err != nil {
		t.Fatalf("EvaluateToolUses: %v", err)
	}

	for _, id := range []string{"toolu_a", "toolu_b"} {
		v := result.PerToolUse[id]
		if v.Outcome != pipeline.OutcomeHold {
			t.Errorf("%s: Outcome = %q, want Hold", id, v.Outcome)
		}
		if v.HoldKey != "shared-key" {
			t.Errorf("%s: HoldKey = %q, want shared-key", id, v.HoldKey)
		}
	}
}

// TestEvaluateToolUses_AllSkipFallsThroughToAllow pins the default-Allow
// behavior: if no evaluator claims a tool_use, it gets Allow.
func TestEvaluateToolUses_AllSkipFallsThroughToAllow(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{{ID: "toolu_x"}}

	evaluators := []pipeline.ToolUseEvaluator{
		&skipEvaluator{name: "skip1"},
		&skipEvaluator{name: "skip2"},
	}

	result, err := pipeline.EvaluateToolUses(context.Background(), res, tools, evaluators, func(id string) pipeline.ToolUseMutator {
		return &recordingToolUseMutator{id: id}
	})
	if err != nil {
		t.Fatalf("EvaluateToolUses: %v", err)
	}

	if v := result.PerToolUse["toolu_x"]; v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("all-Skip should default to Allow, got %q", v.Outcome)
	}
}

// TestEvaluateToolUses_PropagatesEvaluatorError pins error propagation.
func TestEvaluateToolUses_PropagatesEvaluatorError(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{{ID: "toolu_x"}}

	evaluators := []pipeline.ToolUseEvaluator{
		&erroringEvaluator{name: "exploder"},
	}

	_, err := pipeline.EvaluateToolUses(context.Background(), res, tools, evaluators, func(id string) pipeline.ToolUseMutator {
		return &recordingToolUseMutator{id: id}
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}
