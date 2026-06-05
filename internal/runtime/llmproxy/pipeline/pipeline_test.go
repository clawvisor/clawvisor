package pipeline_test

// This is intentionally a thin smoke test. The plan's Phase 1 validation
// gate is "package compiles; no callers yet." Per the testing principle
// in §5 of llmproxy-refactor-plan.md, we explicitly do NOT pre-emptively
// test every interface method here. Contract tests will arrive
// migration-by-migration as Phases 2–4 wire each operation through real
// implementations.

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// TestPanicMutator_PanicsOnAnyCall pins the Phase 1 invariant: until
// the first real wiring lands, every mutator call panics. Migrating
// away from PanicMutator should require updating *this* test to assert
// the new behavior — keeping the surface honest as it grows.
func TestPanicMutator_PanicsOnAnyCall(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic, got nil")
		}
	}()
	var m pipeline.PanicMutator
	_ = m.InjectSystemNotice("x")
}

// TestVerdictOutcomes_Recognized ensures the Outcome string constants
// keep matching the names audit consumers already use today. If we ever
// rename one, this test forces us to acknowledge the audit-row impact.
func TestVerdictOutcomes_Recognized(t *testing.T) {
	cases := []struct {
		got  pipeline.Outcome
		want string
	}{
		{pipeline.OutcomeAllow, "allow"},
		{pipeline.OutcomeDeny, "deny"},
		{pipeline.OutcomeHold, "hold"},
		{pipeline.OutcomeRewrite, "rewrite"},
		{pipeline.OutcomeShortCircuit, "short_circuit"},
		{pipeline.OutcomeSkip, "skip"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("outcome %q = %q, want %q", c.want, string(c.got), c.want)
		}
	}
}
