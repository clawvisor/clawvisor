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

func TestMemoryTransientBudget_FirstTryWinsSecondTryLoses(t *testing.T) {
	b := NewMemoryTransientBudget(time.Minute)
	ctx := context.Background()
	k := key("agent-1", "conv-1", "class-x")
	if !b.Try(ctx, k) {
		t.Fatalf("first Try should return true (budget remaining)")
	}
	if b.Try(ctx, k) {
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
		if !b.Try(ctx, k) {
			t.Fatalf("first attempt for %+v should pass", k)
		}
	}
	for _, k := range keys {
		if b.Try(ctx, k) {
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
	if !b.Try(ctx, k1) {
		t.Fatalf("k1 first attempt should pass")
	}
	if !b.Try(ctx, k2) {
		t.Fatalf("k2 first attempt should pass independently — pipe in IDs must not alias keys")
	}
}

func TestMemoryTransientBudget_TTLExpiryRestoresBudget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	b := &memoryTransientBudget{
		ttl:     time.Minute,
		now:     func() time.Time { return now },
		entries: map[TransientBudgetKey]time.Time{},
	}
	ctx := context.Background()
	k := key("a", "c", "class")
	if !b.Try(ctx, k) {
		t.Fatalf("first attempt should pass")
	}
	if b.Try(ctx, k) {
		t.Fatalf("second attempt before TTL should fail")
	}
	now = now.Add(time.Minute + time.Second)
	if !b.Try(ctx, k) {
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
	if !b.Try(ctx, k) {
		t.Fatal("first attempt should pass")
	}
	if b.Try(ctx, k) {
		t.Fatal("second attempt before release should fail")
	}
	b.Release(ctx, k)
	if !b.Try(ctx, k) {
		t.Fatalf("after Release the slot should be available again")
	}
}

// Release of an unknown key is a no-op (doesn't crash, doesn't poison
// other slots). Important because postproc may roll back release sets
// that include keys the budget never saw if the verdict ordering shifted.
func TestMemoryTransientBudget_ReleaseUnknownKeyIsNoOp(t *testing.T) {
	b := NewMemoryTransientBudget(time.Minute)
	ctx := context.Background()
	b.Release(ctx, key("a", "c", "class")) // never tried
	consumed := key("a", "c", "consumed")
	if !b.Try(ctx, consumed) {
		t.Fatal("unrelated key should still pass after spurious Release")
	}
	if b.Try(ctx, consumed) {
		t.Fatal("budget for consumed key should be intact")
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
			if b.Try(ctx, k) {
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
	if b.Try(context.Background(), key("a", "c", "class")) {
		t.Fatalf("nil receiver should return false (no budget)")
	}
	b.Release(context.Background(), key("a", "c", "class")) // must not panic
}
