package slack

import (
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

// Channel membership must never confer approval rights on its own — that is
// the core authorization property of posting approvals into a shared channel.
func TestCanApprove(t *testing.T) {
	cfg := notify.SlackConfig{
		InstallerSlackUserID: "U_INSTALLER",
		Approvers: []notify.SlackApprover{
			{SlackUserID: "U_ALICE", DisplayName: "alice"},
			{SlackUserID: "U_BOB", DisplayName: "bob"},
		},
	}

	for _, tc := range []struct {
		name string
		user string
		want bool
	}{
		{"installer is implicitly allowed", "U_INSTALLER", true},
		{"allowlisted user", "U_ALICE", true},
		{"another allowlisted user", "U_BOB", true},
		{"channel member not on the allowlist", "U_MALLORY", false},
		{"empty user id", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg.CanApprove(tc.user); got != tc.want {
				t.Fatalf("CanApprove(%q) = %v, want %v", tc.user, got, tc.want)
			}
		})
	}
}

// A config with no installer and no allowlist must reject everyone rather
// than falling open.
func TestCanApprove_EmptyConfigRejectsAll(t *testing.T) {
	var cfg notify.SlackConfig
	for _, u := range []string{"", "U_ANYONE", "U_INSTALLER"} {
		if cfg.CanApprove(u) {
			t.Fatalf("empty config approved %q", u)
		}
	}
}

func TestTokenStore_PeekDoesNotRetireToken(t *testing.T) {
	s := newCallbackTokenStore()
	approve, _, err := s.Generate("approval", "req-1", "user-1", "", "C123", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Peeking repeatedly — as an unauthorized clicker would cause — must
	// leave the token usable for the real approver.
	for i := 0; i < 3; i++ {
		if _, err := s.Peek(approve); err != nil {
			t.Fatalf("peek %d failed: %v", i, err)
		}
	}
	if _, err := s.Consume(approve); err != nil {
		t.Fatalf("consume after peeks failed: %v", err)
	}
}

// Approve and deny share one entry, so resolving either must retire both —
// otherwise a request could be approved and then also denied.
func TestTokenStore_ConsumeRetiresBothSides(t *testing.T) {
	s := newCallbackTokenStore()
	approve, deny, err := s.Generate("approval", "req-1", "user-1", "task-1", "C123", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	entry, err := s.Consume(approve)
	if err != nil {
		t.Fatalf("first consume failed: %v", err)
	}
	if entry.TargetID != "req-1" || entry.TaskID != "task-1" || entry.UserID != "user-1" {
		t.Fatalf("entry did not round-trip: %+v", entry)
	}

	if _, err := s.Consume(deny); err == nil {
		t.Fatal("deny token still usable after approve was consumed")
	}
	if _, err := s.Peek(deny); err == nil {
		t.Fatal("deny token still peekable after approve was consumed")
	}
}

func TestTokenStore_RejectsExpiredAndUnknown(t *testing.T) {
	s := newCallbackTokenStore()
	approve, _, err := s.Generate("approval", "req-1", "user-1", "", "C123", -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Peek(approve); err != errTokenExpired {
		t.Fatalf("expired token: got %v, want %v", err, errTokenExpired)
	}
	if _, err := s.Consume("never-issued"); err != errTokenNotFound {
		t.Fatalf("unknown token: got %v, want %v", err, errTokenNotFound)
	}
}

// Tokens are the authorization for a decision, so they must be unguessable
// and never repeat across targets.
func TestTokenStore_GeneratesDistinctTokens(t *testing.T) {
	s := newCallbackTokenStore()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		a, d, err := s.Generate("approval", "req", "user", "", "C123", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{a, d} {
			if len(tok) != 32 {
				t.Fatalf("token %q is %d chars, want 32 hex chars", tok, len(tok))
			}
			if seen[tok] {
				t.Fatalf("token %q was issued twice", tok)
			}
			seen[tok] = true
		}
	}
}

// A timed-out request must stay distinguishable from an unknown one. Cleanup
// used to delete on expiry, so a late click reported the generic "no longer
// available" instead of saying the request had expired.
func TestTokenStore_ExpiredEntrySurvivesCleanup(t *testing.T) {
	s := newCallbackTokenStore()
	approve, deny, err := s.Generate("approval", "req-1", "user-1", "", "C123", -time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	s.Cleanup()

	for _, tok := range []string{approve, deny} {
		if _, err := s.Peek(tok); err != errTokenExpired {
			t.Fatalf("after cleanup: got %v, want %v", err, errTokenExpired)
		}
	}
}

// A resolved request must keep reporting as resolved rather than decaying
// into "unknown", or the message would be replaced with an expiry notice
// that overwrites its real outcome.
func TestTokenStore_UsedEntrySurvivesCleanup(t *testing.T) {
	s := newCallbackTokenStore()
	approve, _, err := s.Generate("approval", "req-1", "user-1", "", "C123", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(approve); err != nil {
		t.Fatal(err)
	}

	s.Cleanup()

	if _, err := s.Peek(approve); err != errTokenUsed {
		t.Fatalf("after cleanup: got %v, want %v", err, errTokenUsed)
	}
}

// Tombstones must not accumulate forever.
func TestTokenStore_CleanupDropsEntriesPastGrace(t *testing.T) {
	s := newCallbackTokenStore()
	approve, _, err := s.Generate("approval", "req-1", "user-1", "", "C123", -(tombstoneGrace + time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	s.Cleanup()

	if _, err := s.Peek(approve); err != errTokenNotFound {
		t.Fatalf("past grace: got %v, want %v", err, errTokenNotFound)
	}
}
