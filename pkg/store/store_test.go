package store

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// TestUserVerified guards the auth-gate predicate: a NULL (nil) or zero
// VerifiedAt is pending per the struct contract; only a non-zero timestamp
// counts as verified.
func TestUserVerified(t *testing.T) {
	nonZero := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var zero time.Time
	cases := []struct {
		name string
		u    *User
		want bool
	}{
		{"nil user", nil, false},
		{"nil VerifiedAt (NULL)", &User{}, false},
		{"zero VerifiedAt", &User{VerifiedAt: &zero}, false},
		{"non-zero VerifiedAt", &User{VerifiedAt: &nonZero}, true},
	}
	for _, c := range cases {
		if got := c.u.Verified(); got != c.want {
			t.Errorf("%s: Verified()=%v want %v", c.name, got, c.want)
		}
	}
}

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

func TestInstallContextRoundTripsUnknownFields(t *testing.T) {
	// A caller posts a richer install_context than the typed schema knows
	// about — e.g. a future probe section adds model_id/provider/remote_host.
	// The typed fields should populate as usual; the unknown fields should
	// land in Extra; the round-tripped JSON should contain both.
	in := []byte(`{
		"harness": "openclaw",
		"install_mode": "remote",
		"host_os": "darwin",
		"model_id": "claude-sonnet-4-6",
		"provider": "anthropic",
		"remote_host": "user@host.example.com",
		"weird_bool": true,
		"weird_array": [1, 2, 3]
	}`)

	var ic InstallContext
	if err := json.Unmarshal(in, &ic); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if ic.Harness != "openclaw" || ic.InstallMode != "remote" || ic.HostOS != "darwin" {
		t.Fatalf("typed fields not populated: %+v", ic)
	}
	wantExtra := map[string]any{
		"model_id":    "claude-sonnet-4-6",
		"provider":    "anthropic",
		"remote_host": "user@host.example.com",
		"weird_bool":  true,
		"weird_array": []any{float64(1), float64(2), float64(3)},
	}
	if !reflect.DeepEqual(ic.Extra, wantExtra) {
		t.Fatalf("Extra mismatch:\n want: %#v\n got:  %#v", wantExtra, ic.Extra)
	}

	// Marshal should preserve every input key.
	out, err := json.Marshal(ic)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("round-trip Unmarshal: %v", err)
	}
	for _, want := range []string{"harness", "install_mode", "host_os", "model_id", "provider", "remote_host", "weird_bool", "weird_array"} {
		if _, ok := round[want]; !ok {
			t.Errorf("round-tripped JSON missing key %q; got %v", want, round)
		}
	}

	// Re-decode the marshaled output and check the second hop preserves
	// shape — this is the actual code path the store layer uses.
	var ic2 InstallContext
	if err := json.Unmarshal(out, &ic2); err != nil {
		t.Fatalf("Unmarshal round 2: %v", err)
	}
	if ic2.Harness != ic.Harness || ic2.InstallMode != ic.InstallMode || ic2.HostOS != ic.HostOS {
		t.Fatalf("typed fields lost on second decode:\n want: %+v\n got:  %+v", ic, ic2)
	}
	if !reflect.DeepEqual(ic2.Extra, ic.Extra) {
		t.Fatalf("Extra lost on second decode:\n want: %#v\n got:  %#v", ic.Extra, ic2.Extra)
	}
}

func TestInstallContextExtraCannotShadowKnownFields(t *testing.T) {
	// Even if a caller stuffs a "harness" key into Extra by hand (the typed
	// API allows it), Marshal should not let it overwrite the typed field on
	// the way out.
	ic := InstallContext{
		Harness: "openclaw",
		Extra:   map[string]any{"harness": "hijack", "install_mode": "hijack"},
	}
	out, err := json.Marshal(ic)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if round["harness"] != "openclaw" {
		t.Errorf("expected harness=openclaw, got %v", round["harness"])
	}
}

func TestInstallContextIsEmpty(t *testing.T) {
	if !(InstallContext{}).IsEmpty() {
		t.Errorf("zero value should be empty")
	}
	if (InstallContext{Harness: "openclaw"}).IsEmpty() {
		t.Errorf("populated typed field should not be empty")
	}
	if (InstallContext{Extra: map[string]any{"k": "v"}}).IsEmpty() {
		t.Errorf("populated Extra should not be empty")
	}
}
