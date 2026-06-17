package pipelineeval

import "testing"

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
