package pipelineeval

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
)

// TestAppliesToSource_CoversScopeDriftAndIntentRefusal pins which
// decision sources route through the menu. All three menu options
// (expand / new_task / one_off) are reasonable recoveries from either
// a missing/ambiguous task scope OR an intent-verifier refusal: in
// each case the agent's task envelope is the wrong shape for the
// call. Layer 2 hardcoded rules (SourceRuleReview etc.) and decision
// errors stay on the legacy user-prompt path because they're not
// recoverable by the agent.
func TestAppliesToSource_CoversScopeDriftAndIntentRefusal(t *testing.T) {
	t.Parallel()
	c := &scopeDriftCoordinator{}
	cases := []struct {
		source runtimedecision.DecisionSource
		want   bool
	}{
		{runtimedecision.SourceTaskScopeMissing, true},
		{runtimedecision.SourceTaskScopeAmbiguous, true},
		{runtimedecision.SourceIntentRefusal, true},
		// Layer 2 rule-based — not scope drift.
		{runtimedecision.SourceRuleAllow, false},
		{runtimedecision.SourceRuleDeny, false},
		{runtimedecision.SourceRuleReview, false},
		// Allow source — not a denial at all.
		{runtimedecision.SourceTaskScope, false},
	}
	for _, tc := range cases {
		if got := c.AppliesToSource(tc.source); got != tc.want {
			t.Errorf("AppliesToSource(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestDriftSourceFor_DistinguishesIntentFromTaskScope verifies the
// registry's ScopeDriftSource tag matches the decision source so
// telemetry can disambiguate.
func TestDriftSourceFor_DistinguishesIntentFromTaskScope(t *testing.T) {
	t.Parallel()
	if got := driftSourceFor(runtimedecision.SourceIntentRefusal); got != llmproxy.ScopeDriftSourceIntentVerification {
		t.Errorf("driftSourceFor(intent_refusal) = %q, want %q", got, llmproxy.ScopeDriftSourceIntentVerification)
	}
	for _, src := range []runtimedecision.DecisionSource{
		runtimedecision.SourceTaskScopeMissing,
		runtimedecision.SourceTaskScopeAmbiguous,
	} {
		if got := driftSourceFor(src); got != llmproxy.ScopeDriftSourceTaskScope {
			t.Errorf("driftSourceFor(%q) = %q, want %q", src, got, llmproxy.ScopeDriftSourceTaskScope)
		}
	}
}

// TestIsLegacyScopeDriftReason_OnlyActualMissesRoute pins the safety
// whitelist for the legacy TaskScope.Check denial path. Only
// `needs_new_task` and `no_active_task` are genuine scope drift;
// every other denial reason from StoreTaskScopeChecker reflects a
// backend / config / programmer error and must hard-block so an
// operator sees it.
//
// Background: minting a drift for a backend-error denial would let
// an agent ride out a degraded store by picking an option, landing
// a pre-clear on user approval, and bypassing the (still broken)
// scope check on retry — turning recovery into a credential-bypass
// path. The whitelist closes that.
func TestIsLegacyScopeDriftReason_OnlyActualMissesRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		reason string
		want   bool
	}{
		// Genuine scope drift — agent can recover via the menu.
		{"needs_new_task", true},
		{"no_active_task", true},
		// Backend / config / programmer errors — must hard-block.
		{"no_task_store_configured", false},
		{"no_agent_context", false},
		{"unresolved_action", false},
		{"task_store_unavailable", false},
		{"unknown_classification", false},
		// Defensive: an unrecognized reason from a future failure mode
		// defaults to hard-block (safer bias).
		{"", false},
		{"future_reason_that_doesnt_exist_yet", false},
	}
	for _, tc := range cases {
		if got := isLegacyScopeDriftReason(tc.reason); got != tc.want {
			t.Errorf("isLegacyScopeDriftReason(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}
