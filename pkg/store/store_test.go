package store

import "testing"

func TestValidateConversationAutoApproveThreshold(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		enforceCap  bool
		want        string
		expectError bool
	}{
		{"empty_collapses_to_off", "", true, "off", false},
		{"whitespace_collapses_to_off", "   ", true, "off", false},
		{"off_is_valid", "off", true, "off", false},
		{"low_is_valid", "low", true, "low", false},
		{"medium_is_valid_at_cap", "medium", true, "medium", false},
		{"high_rejected_under_cap", "high", true, "", true},
		{"critical_rejected_under_cap", "critical", true, "", true},
		{"high_accepted_without_cap", "high", false, "high", false},
		{"critical_accepted_without_cap", "critical", false, "critical", false},
		{"case_insensitive", "MEDIUM", true, "medium", false},
		{"unknown_value_rejected", "extreme", true, "", true},
		{"unknown_value_rejected_no_cap", "extreme", false, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateConversationAutoApproveThreshold(tc.raw, tc.enforceCap)
			if tc.expectError {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConversationAutoApproveCovers(t *testing.T) {
	cases := []struct {
		name      string
		threshold string
		risk      string
		want      bool
	}{
		// off never covers anything — auto-approval disabled.
		{"off_vs_low", "off", "low", false},
		{"off_vs_critical", "off", "critical", false},
		// low covers only low.
		{"low_vs_low", "low", "low", true},
		{"low_vs_medium", "low", "medium", false},
		{"low_vs_high", "low", "high", false},
		// medium covers low + medium.
		{"medium_vs_low", "medium", "low", true},
		{"medium_vs_medium", "medium", "medium", true},
		{"medium_vs_high", "medium", "high", false},
		// high (theoretical / above UI cap) covers low + medium + high.
		{"high_vs_high", "high", "high", true},
		{"high_vs_critical", "high", "critical", false},
		// critical covers everything.
		{"critical_vs_critical", "critical", "critical", true},
		// Unknown risk levels (e.g. assessor returned "unknown")
		// never auto-approve — fall back to human.
		{"medium_vs_unknown", "medium", "unknown", false},
		// Unknown thresholds never cover — defensive.
		{"garbage_vs_low", "garbage", "low", false},
		// Empty threshold (zero value on a fresh User) treated as off.
		{"empty_vs_low", "", "low", false},
		// Case insensitivity on both sides.
		{"upper_threshold", "MEDIUM", "low", true},
		{"upper_risk", "medium", "LOW", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ConversationAutoApproveCovers(tc.threshold, tc.risk)
			if got != tc.want {
				t.Errorf("Covers(threshold=%q, risk=%q) = %v, want %v",
					tc.threshold, tc.risk, got, tc.want)
			}
		})
	}
}
