package llmproxy

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryTransientBudget_FirstTryWinsSecondTryLoses(t *testing.T) {
	b := NewMemoryTransientBudget(time.Minute)
	ctx := context.Background()
	if !b.Try(ctx, "agent-1", "conv-1", "class-x") {
		t.Fatalf("first Try should return true (budget remaining)")
	}
	if b.Try(ctx, "agent-1", "conv-1", "class-x") {
		t.Fatalf("second Try should return false (budget consumed)")
	}
}

func TestMemoryTransientBudget_DistinctKeysHaveIndependentBudgets(t *testing.T) {
	b := NewMemoryTransientBudget(time.Minute)
	ctx := context.Background()
	if !b.Try(ctx, "agent-1", "conv-1", "class-a") {
		t.Fatalf("class-a first attempt should pass")
	}
	if !b.Try(ctx, "agent-1", "conv-1", "class-b") {
		t.Fatalf("class-b first attempt should pass independently of class-a")
	}
	if !b.Try(ctx, "agent-1", "conv-2", "class-a") {
		t.Fatalf("different conversation should have its own budget for class-a")
	}
	if !b.Try(ctx, "agent-2", "conv-1", "class-a") {
		t.Fatalf("different agent should have its own budget for class-a")
	}
	// Re-trying any of those should now fail.
	for _, args := range [][3]string{
		{"agent-1", "conv-1", "class-a"},
		{"agent-1", "conv-1", "class-b"},
		{"agent-1", "conv-2", "class-a"},
		{"agent-2", "conv-1", "class-a"},
	} {
		if b.Try(ctx, args[0], args[1], args[2]) {
			t.Fatalf("retry %v should be denied (budget consumed)", args)
		}
	}
}

func TestMemoryTransientBudget_TTLExpiryRestoresBudget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	b := &memoryTransientBudget{
		ttl:     time.Minute,
		now:     func() time.Time { return now },
		entries: map[string]time.Time{},
	}
	ctx := context.Background()
	if !b.Try(ctx, "a", "c", "class") {
		t.Fatalf("first attempt should pass")
	}
	if b.Try(ctx, "a", "c", "class") {
		t.Fatalf("second attempt before TTL should fail")
	}
	now = now.Add(time.Minute + time.Second)
	if !b.Try(ctx, "a", "c", "class") {
		t.Fatalf("after TTL expiry, budget should be restored")
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
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			if b.Try(ctx, "agent", "conv", "class") {
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

func TestMemoryTransientBudget_NilReceiverReturnsFalse(t *testing.T) {
	var b *memoryTransientBudget
	if b.Try(context.Background(), "a", "c", "class") {
		t.Fatalf("nil receiver should return false (no budget)")
	}
}
