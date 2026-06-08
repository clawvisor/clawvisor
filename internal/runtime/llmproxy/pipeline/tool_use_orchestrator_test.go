package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// allowEvaluator returns Allow for every tool_use with a tag in Reason.
type allowEvaluator struct {
	name string
	tag  string
}

func (e *allowEvaluator) Name() string { return e.name }
func (e *allowEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, _ conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeAllow,
		Reason:  e.tag,
	}, nil
}

// skipEvaluator returns Skip so later evaluators get a turn.
type skipEvaluator struct{ name string }

func (e *skipEvaluator) Name() string { return e.name }
func (e *skipEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, _ conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
}

type skipWithInspectorFactEvaluator struct {
	name string
	fact pipeline.InspectorFact
}

func (e *skipWithInspectorFactEvaluator) Name() string { return e.name }
func (e *skipWithInspectorFactEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, _ conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeSkip,
		Facts:   []pipeline.EvaluationFact{e.fact},
	}, nil
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

type deniedContinueEvaluator struct {
	name       string
	substitute string
}

func (e *deniedContinueEvaluator) Name() string { return e.name }
func (e *deniedContinueEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, _ conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{
		Outcome:        pipeline.OutcomeDeny,
		SubstituteWith: e.substitute,
		Continue: &pipeline.ContinueSignal{
			SyntheticToolResults: []json.RawMessage{json.RawMessage(`"continued"`)},
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
		if v.Reason != "claimed" {
			t.Errorf("%s: evaluator tag = %v, want claimed", tu.ID, v.Reason)
		}
	}

	// Per-tool-use × per-evaluator trail: first_skip + claimer for each
	// tool_use (later doesn't run). 2 × 2 = 4.
	if len(result.Evaluations) != 4 {
		t.Errorf("expected 4 evaluations in trail, got %d", len(result.Evaluations))
	}
}

func TestEvaluateToolUses_RejectsDuplicateToolUseIDs(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{
		{ID: "toolu_dup", Name: "Bash", Input: json.RawMessage(`{"command":"echo one"}`)},
		{ID: "toolu_dup", Name: "Bash", Input: json.RawMessage(`{"command":"echo two"}`)},
	}

	_, err := pipeline.EvaluateToolUses(context.Background(), res, tools, []pipeline.ToolUseEvaluator{
		&allowEvaluator{name: "claimer", tag: "claimed"},
	}, func(id string) pipeline.ToolUseMutator {
		return &recordingToolUseMutator{id: id}
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate tool_use id") {
		t.Fatalf("EvaluateToolUses error = %v, want duplicate tool_use id error", err)
	}
}

// TestEvaluateToolUses_ContinueShortCircuits pins continuation
// semantics: a Continue signal halts evaluator execution, and
// remaining siblings get explicit deny verdicts so malformed multi-tool
// output cannot bypass later inspector / authorization evaluators.
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
	if v, ok := result.PerToolUse["toolu_should_not_run"]; !ok {
		t.Errorf("tool_use after Continue should get an explicit sibling verdict")
	} else if v.Outcome != pipeline.OutcomeDeny {
		t.Errorf("sibling verdict = %+v, want Deny", v)
	}
}

func TestEvaluateToolUses_DeniedContinueRequiresSubstituteFallback(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{{ID: "toolu_local", Name: "Bash"}}

	_, err := pipeline.EvaluateToolUses(context.Background(), res, tools, []pipeline.ToolUseEvaluator{
		&deniedContinueEvaluator{name: "local_answer"},
	}, func(id string) pipeline.ToolUseMutator {
		return &recordingToolUseMutator{id: id}
	})
	if err == nil {
		t.Fatal("expected denied Continue without SubstituteWith to fail")
	}

	result, err := pipeline.EvaluateToolUses(context.Background(), res, tools, []pipeline.ToolUseEvaluator{
		&deniedContinueEvaluator{name: "local_answer", substitute: "continued"},
	}, func(id string) pipeline.ToolUseMutator {
		return &recordingToolUseMutator{id: id}
	})
	if err != nil {
		t.Fatalf("denied Continue with SubstituteWith should be valid: %v", err)
	}
	if result.Continue == nil {
		t.Fatal("expected Continue set")
	}
	if v := result.PerToolUse["toolu_local"]; v.SubstituteWith != "continued" {
		t.Fatalf("SubstituteWith = %q, want continued", v.SubstituteWith)
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

func TestEvaluateToolUses_UnclaimedAmbiguousCredentialedFactFailsClosed(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{{ID: "toolu_ambiguous", Name: "Bash"}}

	result, err := pipeline.EvaluateToolUses(context.Background(), res, tools, []pipeline.ToolUseEvaluator{
		&skipWithInspectorFactEvaluator{name: "inspector", fact: pipeline.InspectorFact{IsAPICall: true, Ambiguous: true, Host: "api.github.com"}},
	}, func(id string) pipeline.ToolUseMutator {
		return &recordingToolUseMutator{id: id}
	})
	if err != nil {
		t.Fatalf("EvaluateToolUses: %v", err)
	}
	if v := result.PerToolUse["toolu_ambiguous"]; v.Outcome != pipeline.OutcomeDeny {
		t.Fatalf("unclaimed ambiguous credentialed fact verdict = %+v, want Deny", v)
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

	if v := result.PerToolUse["toolu_x"]; v.Outcome != pipeline.OutcomeDeny {
		t.Errorf("all-Skip should default to Deny, got %q", v.Outcome)
	}
}

func TestEvaluateToolUses_AllSkipNonCredentialedInspectorFactDenies(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{{ID: "toolu_local", Name: "Bash"}}

	result, err := pipeline.EvaluateToolUses(context.Background(), res, tools, []pipeline.ToolUseEvaluator{
		&skipWithInspectorFactEvaluator{
			name: "inspector_chain",
			fact: pipeline.InspectorFact{
				Source:    "trigger_miss",
				IsAPICall: false,
				Reason:    "no credential trigger",
			},
		},
		&skipEvaluator{name: "credential_rewrite"},
	}, func(id string) pipeline.ToolUseMutator {
		return &recordingToolUseMutator{id: id}
	})
	if err != nil {
		t.Fatalf("EvaluateToolUses: %v", err)
	}

	if v := result.PerToolUse["toolu_local"]; v.Outcome != pipeline.OutcomeDeny {
		t.Fatalf("non-credentialed all-Skip Outcome = %q, want Deny", v.Outcome)
	}
}

func TestEvaluateToolUses_AllSkipCredentialedAPIDenies(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{{ID: "toolu_cred"}}

	result, err := pipeline.EvaluateToolUses(context.Background(), res, tools, []pipeline.ToolUseEvaluator{
		&skipWithInspectorFactEvaluator{
			name: "inspector_chain",
			fact: pipeline.InspectorFact{
				IsAPICall:    true,
				Placeholders: []string{"autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
				Host:         "api.github.com",
			},
		},
		&skipEvaluator{name: "credential_rewrite"},
	}, func(id string) pipeline.ToolUseMutator {
		return &recordingToolUseMutator{id: id}
	})
	if err != nil {
		t.Fatalf("EvaluateToolUses: %v", err)
	}

	v := result.PerToolUse["toolu_cred"]
	if v.Outcome != pipeline.OutcomeDeny {
		t.Fatalf("credentialed all-Skip Outcome = %q, want Deny", v.Outcome)
	}
	if v.Reason == "" {
		t.Fatal("credentialed default Deny should include reason")
	}
}

func TestEvaluateToolUses_AllSkipAmbiguousAPIFailsClosed(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{{ID: "toolu_ambiguous"}}

	result, err := pipeline.EvaluateToolUses(context.Background(), res, tools, []pipeline.ToolUseEvaluator{
		&skipWithInspectorFactEvaluator{
			name: "inspector_chain",
			fact: pipeline.InspectorFact{
				IsAPICall: true,
				Ambiguous: true,
				Host:      "api.github.com",
			},
		},
	}, func(id string) pipeline.ToolUseMutator {
		return &recordingToolUseMutator{id: id}
	})
	if err != nil {
		t.Fatalf("EvaluateToolUses: %v", err)
	}

	if v := result.PerToolUse["toolu_ambiguous"]; v.Outcome != pipeline.OutcomeDeny {
		t.Fatalf("ambiguous all-Skip Outcome = %q, want Deny", v.Outcome)
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
