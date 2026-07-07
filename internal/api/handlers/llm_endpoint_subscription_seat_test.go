package handlers

import "testing"

// TestDecideSubscriptionSeat locks the fixed precedence for a subscription/
// unrecognized bearer under vault posture: billing-migration consent wins
// first, then the strict govern_subscription_seats=false refuse, else the
// default auto-govern (forward the seat's own credential + enforce).
func TestDecideSubscriptionSeat(t *testing.T) {
	cases := []struct {
		name           string
		allowMigration bool
		governSeats    bool
		want           subscriptionSeatDecision
	}{
		{"default_auto_govern", false, true, subSeatAutoGovern},
		{"strict_refuse", false, false, subSeatRefuse},
		// Migration consent takes precedence over BOTH the auto-govern default
		// and the strict opt-out — an operator who set allow_subscription_billing_migration
		// gets migration regardless of govern_subscription_seats.
		{"migration_wins_over_auto_govern", true, true, subSeatMigrate},
		{"migration_wins_over_strict_refuse", true, false, subSeatMigrate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideSubscriptionSeat(tc.allowMigration, tc.governSeats); got != tc.want {
				t.Fatalf("decideSubscriptionSeat(migration=%v, govern=%v) = %d, want %d",
					tc.allowMigration, tc.governSeats, got, tc.want)
			}
		})
	}
}
