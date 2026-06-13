package policies_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

type evalToolUseMutator struct{}

func (evalToolUseMutator) RewriteArgs(json.RawMessage) error { return nil }
func (evalToolUseMutator) ReplaceWithText(string) error      { return nil }

// TestInspectorEvaluator_SkipsWhenNilInspector pins the gate.
func TestInspectorEvaluator_SkipsWhenNilInspector(t *testing.T) {
	e := policies.NewInspectorEvaluator(nil)
	tu := conversation.ToolUse{ID: "toolu_1", Name: "Bash", Input: json.RawMessage(`{}`)}
	v, err := e.Evaluate(context.Background(), nil, tu, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("nil inspector → Outcome = %q, want Skip", v.Outcome)
	}
}

// TestInspectorEvaluator_TriggerMissReturnsSkip verifies that an input
// without any autovault placeholder substring → Skip (lets downstream
// evaluators decide).
func TestInspectorEvaluator_TriggerMissReturnsSkip(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	e := policies.NewInspectorEvaluator(insp)

	tu := conversation.ToolUse{
		ID:    "toolu_2",
		Name:  "Bash",
		Input: json.RawMessage(`{"cmd":"ls /tmp"}`),
	}
	v, err := e.Evaluate(context.Background(), nil, tu, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("trigger miss → Outcome = %q, want Skip", v.Outcome)
	}
	if src := inspectorFactSource(v.Facts); src != inspector.SourceTriggerMiss {
		t.Errorf("InspectorFact.Source = %v, want trigger_miss (facts: %+v)", src, v.Facts)
	}
}

func inspectorFactSource(facts []pipeline.EvaluationFact) inspector.VerdictSource {
	for _, f := range facts {
		if ifct, ok := f.(pipeline.InspectorFact); ok {
			return inspector.VerdictSource(ifct.Source)
		}
	}
	return ""
}

func inspectorFactHost(facts []pipeline.EvaluationFact) string {
	for _, f := range facts {
		if ifct, ok := f.(pipeline.InspectorFact); ok {
			return ifct.Host
		}
	}
	return ""
}

func inspectorFactMethod(facts []pipeline.EvaluationFact) string {
	for _, f := range facts {
		if ifct, ok := f.(pipeline.InspectorFact); ok {
			return ifct.Method
		}
	}
	return ""
}

// TestInspectorEvaluator_AmbiguousHolds verifies the fail-closed
// behavior: when the validator can't classify, the policy emits Hold
// (per-tool key — ambiguous-for-different-reasons shouldn't coalesce).
func TestInspectorEvaluator_AmbiguousHolds(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	e := policies.NewInspectorEvaluator(insp)

	// Input that the deterministic parser can't classify AND contains
	// an autovault placeholder so the trigger HITS but the parser misses,
	// forcing fall-through to AmbiguousValidator.
	tu := conversation.ToolUse{
		ID:    "toolu_amb",
		Name:  "unknown_tool",
		Input: json.RawMessage(`{"opaque":"autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`),
	}
	v, err := e.Evaluate(context.Background(), nil, tu, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeHold {
		t.Errorf("ambiguous → Outcome = %q, want Hold", v.Outcome)
	}
	// HoldKey is per-tool so ambiguous siblings don't coalesce.
	if v.HoldKey != "ambiguous_toolu_amb" {
		t.Errorf("HoldKey = %q, want per-tool ambiguous_toolu_amb", v.HoldKey)
	}
}

// TestInspectorEvaluator_AmbiguousAgentRecoverableContinues mirrors the
// InspectorChain symmetric test: a deterministic parser refusal whose
// reason names a fixable shape (here `$(...)` inside a curl pipeline)
// must route through RecoverableDenyVerdict so the proxy's one-shot
// continuation retry can feed the reason back as a synthetic
// tool_result. Both inspector entry points must agree, otherwise a
// future caller that wires the evaluator instead of the chain will
// silently regress to human-approval on every trivially-fixable shape
// error.
func TestInspectorEvaluator_AmbiguousAgentRecoverableContinues(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	e := policies.NewInspectorEvaluator(insp)

	tu := conversation.ToolUse{
		ID:   "toolu_evaluator_recoverable",
		Name: "Bash",
		Input: json.RawMessage(`{"cmd":"echo $(curl -sS -H 'Authorization: Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' https://api.github.com/user)"}`),
	}
	v, err := e.Evaluate(context.Background(), nil, tu, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeDeny {
		t.Fatalf("agent-recoverable refusal → Outcome = %q, want Deny (facts: %+v)", v.Outcome, v.Facts)
	}
	if v.Continue == nil || len(v.Continue.SyntheticToolResults) != 1 {
		t.Fatalf("Continue.SyntheticToolResults missing — InspectorEvaluator must wire the one-shot continuation retry on agent-recoverable refusals (parity with InspectorChain)")
	}
	if content, ok := v.ContinuationToolResultContent(); !ok || !strings.Contains(content, "command substitution") {
		t.Errorf("ContinuationToolResultContent = %q, %v; want the parser's refusal reason verbatim", content, ok)
	}
	if v.HoldKey != "" {
		t.Errorf("HoldKey = %q, want empty — recoverable path must not request human approval", v.HoldKey)
	}
}

// TestInspectorEvaluator_AllowOnRecognizedAPICall verifies the positive
// path: a recognized API-call tool_use → Allow with verdict surfaced
// through AuditParams.
func TestInspectorEvaluator_AllowOnRecognizedAPICall(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	e := policies.NewInspectorEvaluator(insp)

	// A WebFetch-style structured call the deterministic parser
	// recognizes. The placeholder appears in the Authorization header.
	tu := conversation.ToolUse{
		ID:   "toolu_api",
		Name: "WebFetch",
		Input: json.RawMessage(`{
			"url":"https://api.github.com/repos/x/y/issues",
			"method":"GET",
			"headers":{"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}
		}`),
	}
	v, err := e.Evaluate(context.Background(), nil, tu, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("recognized API call → Outcome = %q, want Allow (facts: %+v)", v.Outcome, v.Facts)
	}
	if !inspectorFactIsAPI(v.Facts) {
		t.Errorf("InspectorFact.IsAPICall = false, want true (facts: %+v)", v.Facts)
	}
	if m := inspectorFactMethod(v.Facts); m != "GET" {
		t.Errorf("InspectorFact.Method = %v, want GET", m)
	}
	if h := inspectorFactHost(v.Facts); h != "api.github.com" {
		t.Errorf("InspectorFact.Host = %v, want api.github.com", h)
	}
}
