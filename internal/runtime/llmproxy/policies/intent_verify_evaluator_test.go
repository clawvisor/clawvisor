package policies_test

import (
	"context"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestIntentVerify_SkipsNilResolver pins the gate.
func TestIntentVerify_SkipsNilResolver(t *testing.T) {
	e := policies.NewIntentVerifyEvaluator(nil)
	v, err := e.Evaluate(context.Background(), nil, conversation.ToolUse{}, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", v.Outcome)
	}
}

// TestIntentVerify_SkipOnVerifierPass pins the success path.
func TestIntentVerify_SkipOnVerifierPass(t *testing.T) {
	resolver := func(_ context.Context, _ conversation.ToolUse) (bool, string) {
		return true, "matches scope"
	}
	e := policies.NewIntentVerifyEvaluator(resolver)
	v, err := e.Evaluate(context.Background(), nil, conversation.ToolUse{ID: "toolu_1"}, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("verifier pass → Outcome = %q, want Skip", v.Outcome)
	}
	found := false
	for _, f := range v.Facts {
		if iv, ok := f.(pipeline.IntentVerifyFact); ok && iv.Allowed && iv.Outcome == "intent_verification_passed" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("IntentVerifyFact pass outcome missing (facts: %+v)", v.Facts)
	}
}

// TestIntentVerify_DenyOnVerifierFail pins the fail path: verifier
// says no → Deny.
func TestIntentVerify_DenyOnVerifierFail(t *testing.T) {
	resolver := func(_ context.Context, _ conversation.ToolUse) (bool, string) {
		return false, "tool_use doesn't match task purpose"
	}
	e := policies.NewIntentVerifyEvaluator(resolver)
	v, err := e.Evaluate(context.Background(), nil, conversation.ToolUse{ID: "toolu_2"}, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeDeny {
		t.Errorf("verifier fail → Outcome = %q, want Deny", v.Outcome)
	}
	if v.Reason == "" {
		t.Errorf("Reason should be populated on Deny")
	}
}

// TestIntentVerify_SilentPassSkips pins the (true, "") path.
func TestIntentVerify_SilentPassSkips(t *testing.T) {
	resolver := func(_ context.Context, _ conversation.ToolUse) (bool, string) {
		return true, ""
	}
	e := policies.NewIntentVerifyEvaluator(resolver)
	v, err := e.Evaluate(context.Background(), nil, conversation.ToolUse{}, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("silent pass → Outcome = %q, want Skip", v.Outcome)
	}
}

// TestIntentVerify_EmptyDenyFailsClosed pins the (false, "") path:
// resolver denials with missing explanations still deny.
func TestIntentVerify_EmptyDenyFailsClosed(t *testing.T) {
	resolver := func(_ context.Context, _ conversation.ToolUse) (bool, string) {
		return false, ""
	}
	e := policies.NewIntentVerifyEvaluator(resolver)
	v, err := e.Evaluate(context.Background(), nil, conversation.ToolUse{}, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeDeny {
		t.Errorf("empty deny → Outcome = %q, want Deny", v.Outcome)
	}
	if v.Reason == "" {
		t.Errorf("empty deny should get fallback reason")
	}
}
