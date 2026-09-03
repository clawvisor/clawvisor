package slack

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedisStore returns a Redis-backed store over an in-process server,
// so the atomicity these tests are about is exercised through real Redis
// command semantics rather than a hand-written fake.
func newTestRedisStore(t *testing.T) (*redisCallbackTokenStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &redisCallbackTokenStore{rdb: rdb}, mr
}

// The whole point of the pair guard: a request must never come back both
// approved and denied. Approve and deny are separate keys, so this is the
// case a per-key GetDel got wrong — both halves succeeded and each retired
// the other's already-read token.
func TestRedisTokenStore_ConcurrentApproveAndDenyYieldOneWinner(t *testing.T) {
	s, _ := newTestRedisStore(t)

	// Repeated because a lost race is probabilistic; a non-atomic consume
	// fails this within the first few rounds.
	for round := 0; round < 50; round++ {
		approve, deny, err := s.Generate("approval", "req-1", "user-1", "task-1", "C123", time.Minute)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}

		var (
			start   = make(chan struct{})
			wg      sync.WaitGroup
			mu      sync.Mutex
			winners []string
			losses  []error
		)
		for _, tok := range []string{approve, deny} {
			wg.Add(1)
			go func(tok string) {
				defer wg.Done()
				<-start
				entry, err := s.Consume(tok)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					losses = append(losses, err)
					return
				}
				winners = append(winners, entry.TargetID)
			}(tok)
		}
		close(start)
		wg.Wait()

		if len(winners) != 1 {
			t.Fatalf("round %d: %d consumers won, want exactly 1 — the request would be both approved and denied", round, len(winners))
		}
		if len(losses) != 1 || !errors.Is(losses[0], errTokenUsed) {
			t.Fatalf("round %d: loser saw %v, want errTokenUsed", round, losses)
		}
	}
}

// A late click must learn the accurate reason it did nothing, since that is
// what decides whether the message is repaired or left alone.
func TestRedisTokenStore_ConsumeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup returns the token to consume.
		setup func(t *testing.T, s *redisCallbackTokenStore) string
		want  error
	}{
		{
			name: "fresh token succeeds",
			setup: func(t *testing.T, s *redisCallbackTokenStore) string {
				approve, _, err := s.Generate("approval", "req-1", "user-1", "", "C123", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				return approve
			},
			want: nil,
		},
		{
			name: "same token twice reports used",
			setup: func(t *testing.T, s *redisCallbackTokenStore) string {
				approve, _, err := s.Generate("approval", "req-1", "user-1", "", "C123", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Consume(approve); err != nil {
					t.Fatal(err)
				}
				return approve
			},
			want: errTokenUsed,
		},
		{
			name: "sibling after a decision reports used",
			setup: func(t *testing.T, s *redisCallbackTokenStore) string {
				approve, deny, err := s.Generate("approval", "req-1", "user-1", "", "C123", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Consume(approve); err != nil {
					t.Fatal(err)
				}
				return deny
			},
			want: errTokenUsed,
		},
		{
			name: "expired token reports expired",
			setup: func(t *testing.T, s *redisCallbackTokenStore) string {
				approve, _, err := s.Generate("approval", "req-1", "user-1", "", "C123", -time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				return approve
			},
			want: errTokenExpired,
		},
		{
			name: "unknown token reports not found",
			setup: func(t *testing.T, _ *redisCallbackTokenStore) string {
				return "deadbeef"
			},
			want: errTokenNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestRedisStore(t)
			tok := tc.setup(t, s)

			entry, err := s.Consume(tok)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Consume err = %v, want %v", err, tc.want)
			}
			if tc.want == nil && entry == nil {
				t.Fatal("Consume returned no entry on success")
			}
		})
	}
}

// A token that expired unconsumed must keep reporting itself as expired, not
// decay into "already used" — the two drive different repairs of the message.
func TestRedisTokenStore_ExpiredStaysExpiredOnRepeatedClicks(t *testing.T) {
	s, _ := newTestRedisStore(t)
	approve, deny, err := s.Generate("approval", "req-1", "user-1", "", "C123", -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Consume(approve); !errors.Is(err, errTokenExpired) {
			t.Fatalf("click %d: err = %v, want errTokenExpired", i, err)
		}
	}
	if _, err := s.Peek(deny); !errors.Is(err, errTokenExpired) {
		t.Fatalf("sibling peek err = %v, want errTokenExpired", err)
	}
}

// An unauthorized click must not burn a live approval, so Peek has to leave
// both halves of the pair usable however often it runs.
func TestRedisTokenStore_PeekDoesNotRetireToken(t *testing.T) {
	s, _ := newTestRedisStore(t)
	approve, deny, err := s.Generate("approval", "req-1", "user-1", "task-1", "C123", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := s.Peek(approve); err != nil {
			t.Fatalf("peek %d: %v", i, err)
		}
		if _, err := s.Peek(deny); err != nil {
			t.Fatalf("sibling peek %d: %v", i, err)
		}
	}

	entry, err := s.Consume(deny)
	if err != nil {
		t.Fatalf("consume after peeks: %v", err)
	}
	if entry.TargetID != "req-1" || entry.TaskID != "task-1" || entry.ChannelID != "C123" {
		t.Fatalf("entry did not round-trip: %+v", entry)
	}
}

// Callback tokens live for a day, so a deploy of the guard finds live prompts
// whose tokens predate it. Those must stay clickable rather than reading as
// already resolved.
func TestRedisTokenStore_ConsumesTokensMintedBeforeTheGuard(t *testing.T) {
	s, mr := newTestRedisStore(t)

	legacy := func(sibling string) string {
		b, err := json.Marshal(redisCallbackEntry{
			Type: "approval", TargetID: "req-1", UserID: "user-1", ChannelID: "C123",
			ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), SiblingID: sibling,
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if err := mr.Set(redisTokenPrefix+"old-approve", legacy("old-deny")); err != nil {
		t.Fatal(err)
	}
	if err := mr.Set(redisTokenPrefix+"old-deny", legacy("old-approve")); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Consume("old-approve"); err != nil {
		t.Fatalf("guardless token was not consumable: %v", err)
	}
	// Retiring the sibling still has to hold, or the pre-guard prompt could
	// be approved and then also denied.
	if _, err := s.Consume("old-deny"); !errors.Is(err, errTokenUsed) {
		t.Fatalf("sibling err = %v, want errTokenUsed", err)
	}
}

// Generate must not leave a half-written pair: a token whose guard is missing
// would be read as already-won and could never be resolved.
func TestRedisTokenStore_GenerateWritesGuardWithPair(t *testing.T) {
	s, mr := newTestRedisStore(t)
	approve, deny, err := s.Generate("approval", "req-1", "user-1", "", "C123", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	data, err := mr.Get(redisTokenPrefix + approve)
	if err != nil {
		t.Fatal(err)
	}
	var j redisCallbackEntry
	if err := json.Unmarshal([]byte(data), &j); err != nil {
		t.Fatal(err)
	}
	if j.GuardID == "" {
		t.Fatal("approve token carries no guard id")
	}
	if j.SiblingID != deny {
		t.Fatalf("sibling = %q, want %q", j.SiblingID, deny)
	}
	if _, err := mr.Get(redisGuardPrefix + j.GuardID); err != nil {
		t.Fatalf("guard key missing: %v", err)
	}
	// Both halves must arbitrate on the same key, or neither can lose.
	denyData, err := mr.Get(redisTokenPrefix + deny)
	if err != nil {
		t.Fatal(err)
	}
	var d redisCallbackEntry
	if err := json.Unmarshal([]byte(denyData), &d); err != nil {
		t.Fatal(err)
	}
	if d.GuardID != j.GuardID {
		t.Fatalf("deny guard = %q, want the shared %q", d.GuardID, j.GuardID)
	}
}

// Redis being down must surface as an error rather than a token that looks
// unknown — the handler tells the clicker different things in each case.
func TestRedisTokenStore_ReportsBackendFailure(t *testing.T) {
	s, mr := newTestRedisStore(t)
	approve, _, err := s.Generate("approval", "req-1", "user-1", "", "C123", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mr.Close()

	if _, err := s.Consume(approve); err == nil {
		t.Fatal("Consume succeeded with the backend down")
	}
	if _, err := s.Peek(approve); err == nil {
		t.Fatal("Peek succeeded with the backend down")
	}
}
