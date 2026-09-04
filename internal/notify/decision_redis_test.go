package notify

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

func newTestBus(t *testing.T) (*RedisDecisionBus, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewRedisDecisionBus(rdb, slog.New(slog.NewTextHandler(io.Discard, nil))), mr
}

// BRPOP is a destructive read, so a decision popped by an instance that is
// shutting down exists only in that goroutine. Dropping it loses a human's
// approval with no error and no redelivery — and during a rolling deploy,
// draining instances keep popping right up until their context is cancelled,
// so this is a common path rather than a rare one.
func TestRedisDecisionBus_ShutdownReturnsPoppedDecision(t *testing.T) {
	bus, mr := newTestBus(t)

	if err := bus.Publish(context.Background(), notify.CallbackDecision{
		Type: "task", Action: "approve", TargetID: "task-1", UserID: "user-1",
	}); err != nil {
		t.Fatal(err)
	}

	// Subscribe, then cancel without ever reading from the channel — the
	// shape of an instance draining mid-pop.
	ctx, cancel := context.WithCancel(context.Background())
	bus.Subscribe(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for mr.Exists(redisDecisionQueue) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if mr.Exists(redisDecisionQueue) {
		t.Fatal("decision was never popped; test cannot exercise the shutdown path")
	}

	cancel()

	// It must come back, or it is lost forever.
	for time.Now().Before(deadline) {
		if n, _ := mr.List(redisDecisionQueue); len(n) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("decision was dropped on shutdown instead of returned to the queue")
}

// The ordinary path must still deliver.
func TestRedisDecisionBus_DeliversToSubscriber(t *testing.T) {
	bus, _ := newTestBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := bus.Subscribe(ctx)
	if err := bus.Publish(ctx, notify.CallbackDecision{
		Type: "task", Action: "approve", TargetID: "task-1",
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case d := <-ch:
		if d.TargetID != "task-1" || d.Action != "approve" {
			t.Fatalf("decision did not round-trip: %+v", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("decision never arrived")
	}
}
