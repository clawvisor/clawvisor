package policies_test

import (
	"context"
	"testing"

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
	if result.AuditFields["boundary_check_passed"] != true {
		t.Errorf("boundary_check_passed = %v, want true", result.AuditFields["boundary_check_passed"])
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
	if result.AuditFields["boundary_check_passed"] != false {
		t.Errorf("boundary_check_passed = %v, want false", result.AuditFields["boundary_check_passed"])
	}
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
