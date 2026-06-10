package policies_test

import (
	"context"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestBoundaryCheck_AllowOnMatchedHost verifies the positive path.
func TestBoundaryCheck_AllowOnMatchedHost(t *testing.T) {
	resolver := func(_ context.Context, _ string) []string {
		return []string{"api.github.com"}
	}
	e := policies.NewBoundaryCheckEvaluator(resolver)

	v := inspector.Verdict{
		IsAPICall: true,
		Method:    "GET",
		Host:      "api.github.com",
		Path:      "/repos/x/y",
	}
	result := e.EvaluateWithVerdict(context.Background(), v, []string{"api.github.com"})
	if result.Outcome != pipeline.OutcomeAllow {
		t.Errorf("matched host → Outcome = %q, want Allow", result.Outcome)
	}
	if !boundaryFactPassed(result.Facts) {
		t.Errorf("boundary_check_passed fact = false, want true (facts: %+v)", result.Facts)
	}
}

// TestBoundaryCheck_DenyOnMismatchedHost verifies the negative path:
// a verdict targeting a host not in the allowlist → Deny.
func TestBoundaryCheck_DenyOnMismatchedHost(t *testing.T) {
	resolver := func(_ context.Context, _ string) []string {
		return []string{"api.github.com"}
	}
	e := policies.NewBoundaryCheckEvaluator(resolver)

	v := inspector.Verdict{
		IsAPICall: true,
		Method:    "GET",
		Host:      "evil.example.com",
		Path:      "/exfil",
	}
	result := e.EvaluateWithVerdict(context.Background(), v, []string{"api.github.com"})
	if result.Outcome != pipeline.OutcomeDeny {
		t.Errorf("mismatched host → Outcome = %q, want Deny", result.Outcome)
	}
	if boundaryFactPassed(result.Facts) {
		t.Errorf("boundary_check_passed fact = true, want false (facts: %+v)", result.Facts)
	}
	if result.SubstituteWith == "" || result.SubstituteWith != result.Reason {
		t.Errorf("SubstituteWith = %q, want = Reason for the terminal-fallback path", result.SubstituteWith)
	}
	if result.Continue == nil || len(result.Continue.SyntheticToolResults) != 1 {
		t.Fatalf("Continue.SyntheticToolResults missing — boundary deny should be recoverable so the agent can retry against an allowed host")
	}
	if content, ok := result.ContinuationToolResultContent(); !ok || content != result.Reason {
		t.Errorf("ContinuationToolResultContent = %q, %v; want Reason verbatim", content, ok)
	}
}

func boundaryFactPassed(facts []pipeline.EvaluationFact) bool {
	for _, f := range facts {
		if bf, ok := f.(pipeline.BoundaryFact); ok {
			return bf.Passed
		}
	}
	return false
}

// TestBoundaryCheck_DenyOnAmbiguousVerdict verifies that an ambiguous
// verdict — which BoundaryCheck refuses to act on — returns Deny.
func TestBoundaryCheck_DenyOnAmbiguousVerdict(t *testing.T) {
	resolver := func(_ context.Context, _ string) []string {
		return []string{"api.github.com"}
	}
	e := policies.NewBoundaryCheckEvaluator(resolver)

	v := inspector.Verdict{
		Ambiguous: true,
		Reason:    "unparseable shape",
	}
	result := e.EvaluateWithVerdict(context.Background(), v, []string{"api.github.com"})
	if result.Outcome != pipeline.OutcomeDeny {
		t.Errorf("ambiguous → Outcome = %q, want Deny", result.Outcome)
	}
}

// TestBoundaryCheck_NilResolverSkips pins the gate.
func TestBoundaryCheck_NilResolverSkips(t *testing.T) {
	e := policies.NewBoundaryCheckEvaluator(nil)
	v := inspector.Verdict{IsAPICall: true, Host: "api.github.com"}
	result := e.EvaluateWithVerdict(context.Background(), v, []string{"api.github.com"})
	if result.Outcome != pipeline.OutcomeSkip {
		t.Errorf("nil resolver → Outcome = %q, want Skip", result.Outcome)
	}
}

func TestBoundaryCheck_StandaloneEvaluateErrors(t *testing.T) {
	e := policies.NewBoundaryCheckEvaluator(func(context.Context, string) []string {
		return []string{"api.github.com"}
	})
	_, err := e.Evaluate(context.Background(), nil, conversation.ToolUse{ID: "toolu_1"}, evalToolUseMutator{})
	if err == nil || !strings.Contains(err.Error(), "not valid as a standalone") {
		t.Fatalf("standalone Evaluate error = %v, want explicit invalid-standalone error", err)
	}
}

func TestPendingApprovalHoldPolicy_StandaloneEvaluateErrors(t *testing.T) {
	e := policies.NewPendingApprovalHoldPolicy(nil)
	_, err := e.Evaluate(context.Background(), nil, conversation.ToolUse{ID: "toolu_1"}, evalToolUseMutator{})
	if err == nil || !strings.Contains(err.Error(), "not valid as a standalone") {
		t.Fatalf("standalone Evaluate error = %v, want explicit invalid-standalone error", err)
	}
}
