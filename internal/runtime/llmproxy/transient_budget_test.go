package llmproxy

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func key(agent, conv, class string) TransientBudgetKey {
	return TransientBudgetKey{AgentID: agent, ConversationID: conv, FailureClass: class}
}

// tryOnce is a test helper that adapts the (token, ok) return shape
// to a plain bool when the test doesn't care about the token.
func tryOnce(b TransientBudget, ctx context.Context, k TransientBudgetKey) bool {
	_, ok := b.Try(ctx, k)
	return ok
}

func TestMemoryTransientBudget_FirstTryWinsSecondTryLoses(t *testing.T) {
	b := NewMemoryTransientBudget(time.Minute)
	ctx := context.Background()
	k := key("agent-1", "conv-1", "class-x")
	if !tryOnce(b, ctx, k) {
		t.Fatalf("first Try should return true (budget remaining)")
	}
	if tryOnce(b, ctx, k) {
		t.Fatalf("second Try should return false (budget consumed)")
	}
}

func TestMemoryTransientBudget_DistinctKeysHaveIndependentBudgets(t *testing.T) {
	b := NewMemoryTransientBudget(time.Minute)
	ctx := context.Background()
	keys := []TransientBudgetKey{
		key("agent-1", "conv-1", "class-a"),
		key("agent-1", "conv-1", "class-b"),
		key("agent-1", "conv-2", "class-a"),
		key("agent-2", "conv-1", "class-a"),
	}
	for _, k := range keys {
		if !tryOnce(b, ctx, k) {
			t.Fatalf("first attempt for %+v should pass", k)
		}
	}
	for _, k := range keys {
		if tryOnce(b, ctx, k) {
			t.Fatalf("retry %+v should be denied (budget consumed)", k)
		}
	}
}

// Components that contain a pipe ("agent|x" vs "agent" + "x|...")
// MUST stay isolated. A struct key cannot collide; this guards against
// regressing back to string concatenation.
func TestMemoryTransientBudget_PipeInIDsDoesNotCollide(t *testing.T) {
	b := NewMemoryTransientBudget(time.Minute)
	ctx := context.Background()
	k1 := key("agent|x", "conv", "class")
	k2 := key("agent", "x|conv", "class")
	if !tryOnce(b, ctx, k1) {
		t.Fatalf("k1 first attempt should pass")
	}
	if !tryOnce(b, ctx, k2) {
		t.Fatalf("k2 first attempt should pass independently — pipe in IDs must not alias keys")
	}
}

func TestMemoryTransientBudget_TTLExpiryRestoresBudget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	b := &memoryTransientBudget{
		ttl:     time.Minute,
		now:     func() time.Time { return now },
		entries: map[TransientBudgetKey]transientBudgetEntry{},
	}
	ctx := context.Background()
	k := key("a", "c", "class")
	if !tryOnce(b, ctx, k) {
		t.Fatalf("first attempt should pass")
	}
	if tryOnce(b, ctx, k) {
		t.Fatalf("second attempt before TTL should fail")
	}
	now = now.Add(time.Minute + time.Second)
	if !tryOnce(b, ctx, k) {
		t.Fatalf("after TTL expiry, budget should be restored")
	}
}

// Release refunds a previously-consumed slot so a follow-up Try
// succeeds again. Underpins the postproc rollback that refunds slots
// when a response is fail-closed and the agent never saw the
// recoverable verdict.
func TestMemoryTransientBudget_ReleaseRefundsSlot(t *testing.T) {
	b := NewMemoryTransientBudget(time.Minute)
	ctx := context.Background()
	k := key("a", "c", "class")
	token, ok := b.Try(ctx, k)
	if !ok {
		t.Fatal("first attempt should pass")
	}
	if tryOnce(b, ctx, k) {
		t.Fatal("second attempt before release should fail")
	}
	b.Release(ctx, k, token)
	if !tryOnce(b, ctx, k) {
		t.Fatalf("after Release the slot should be available again")
	}
}

// Release of an unknown key is a no-op (doesn't crash, doesn't poison
// other slots). Important because postproc may roll back release sets
// that include keys the budget never saw if the verdict ordering shifted.
func TestMemoryTransientBudget_ReleaseUnknownKeyIsNoOp(t *testing.T) {
	b := NewMemoryTransientBudget(time.Minute)
	ctx := context.Background()
	b.Release(ctx, key("a", "c", "class"), TransientReleaseToken(1)) // never tried
	consumed := key("a", "c", "consumed")
	if !tryOnce(b, ctx, consumed) {
		t.Fatal("unrelated key should still pass after spurious Release")
	}
	if tryOnce(b, ctx, consumed) {
		t.Fatal("budget for consumed key should be intact")
	}
}

// The race the token guards against: R1 consumes a slot, the entry
// times out and gets pruned, R2 consumes the same key (fresh slot,
// new token), R1's delayed rollback calls Release with R1's stale
// token. The Release MUST be a no-op so R2's slot survives.
func TestMemoryTransientBudget_ReleaseWithStaleTokenIsNoOp(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	b := &memoryTransientBudget{
		ttl:     time.Minute,
		now:     func() time.Time { return now },
		entries: map[TransientBudgetKey]transientBudgetEntry{},
	}
	ctx := context.Background()
	k := key("a", "c", "class")

	r1Token, ok := b.Try(ctx, k)
	if !ok {
		t.Fatal("R1's first attempt should pass")
	}

	// TTL elapses; R2's Try sees the entry pruned and consumes a
	// fresh slot with a new token.
	now = now.Add(time.Minute + time.Second)
	r2Token, ok := b.Try(ctx, k)
	if !ok {
		t.Fatal("R2's first attempt after expiry should pass")
	}
	if r1Token == r2Token {
		t.Fatalf("R1 and R2 must get distinct tokens; both got %d", r1Token)
	}

	// R1's delayed rollback fires with R1's token — must NOT clobber
	// R2's slot.
	b.Release(ctx, k, r1Token)
	if tryOnce(b, ctx, k) {
		t.Fatal("R2's slot must survive R1's stale Release; got an opening for a third Try")
	}

	// R2's own Release (with R2's token) refunds correctly.
	b.Release(ctx, k, r2Token)
	if !tryOnce(b, ctx, k) {
		t.Fatal("after R2's own Release the slot should be available again")
	}
}

func TestMemoryTransientBudget_ConcurrentTryHasExactlyOneWinner(t *testing.T) {
	b := NewMemoryTransientBudget(time.Minute)
	ctx := context.Background()
	const goroutines = 64
	var (
		wg      sync.WaitGroup
		winners atomic.Int64
	)
	start := make(chan struct{})
	k := key("agent", "conv", "class")
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			if tryOnce(b, ctx, k) {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("expected exactly one winner across %d concurrent Try calls; got %d", goroutines, got)
	}
}

func TestMemoryTransientBudget_NilReceiverSafe(t *testing.T) {
	var b *memoryTransientBudget
	if tryOnce(b, context.Background(), key("a", "c", "class")) {
		t.Fatalf("nil receiver should return false (no budget)")
	}
	b.Release(context.Background(), key("a", "c", "class"), TransientReleaseToken(0)) // must not panic
}
