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

func publish(t *testing.T, b *RedisDecisionBus, target string) {
	t.Helper()
	if err := b.Publish(context.Background(), notify.CallbackDecision{
		Type: "task", Action: "approve", TargetID: target, UserID: "user-1",
	}); err != nil {
		t.Fatal(err)
	}
}

// The property the old design lacked: a decision taken but never acked is
// still recoverable. Previously it was popped destructively, so a handler
// that never finished — a scale-down, a rolling deploy — lost it silently.
func TestDecisionBus_UnackedDecisionIsRedelivered(t *testing.T) {
	bus, mr := newTestBus(t)
	publish(t, bus, "task-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := bus.Subscribe(ctx)

	select {
	case <-ch: // take it, deliberately never ack — the handler "died"
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery")
	}

	// Taken, so off the queue — but held on the processing list rather
	// than living only in the handler's memory.
	if q, _ := mr.List(redisDecisionQueue); len(q) != 0 {
		t.Fatalf("queue should be empty while in flight, has %d", len(q))
	}
	inflight, _ := mr.List(redisDecisionProcessing)
	if len(inflight) != 1 {
		t.Fatal("decision is not on the processing list; it would be lost if this instance stopped")
	}

	// Age the in-flight marker past the visibility timeout. miniredis's
	// FastForward moves its own clock, not the wall clock the scores use,
	// so the entry is rewritten directly.
	if err := bus.rdb.ZAdd(context.Background(), redisDecisionStarted,
		redis.Z{Score: float64(time.Now().Add(-2 * visibilityTimeout).Unix()), Member: inflight[0]}).Err(); err != nil {
		t.Fatal(err)
	}
	bus.reclaim(context.Background())

	// The decision comes back rather than being lost.
	select {
	case d := <-ch:
		if d.Decision.TargetID != "task-1" {
			t.Fatalf("redelivered the wrong decision: %+v", d.Decision)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an unacked decision was never redelivered; it is lost")
	}
}

// Acking must retire it, or every decision would be handled twice.
func TestDecisionBus_AckRetiresTheDecision(t *testing.T) {
	bus, mr := newTestBus(t)
	publish(t, bus, "task-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := bus.Subscribe(ctx)

	select {
	case d := <-ch:
		d.Ack()
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p, _ := mr.List(redisDecisionProcessing)
		if len(p) == 0 {
			bus.reclaim(context.Background())
			if q, _ := mr.List(redisDecisionQueue); len(q) != 0 {
				t.Fatal("an acked decision was redelivered")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ack did not clear the processing list")
}

// A decision still within its visibility timeout is being handled, not lost.
func TestDecisionBus_ReclaimLeavesInFlightDecisionsAlone(t *testing.T) {
	bus, mr := newTestBus(t)
	publish(t, bus, "task-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := bus.Subscribe(ctx)
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery")
	}

	bus.reclaim(context.Background()) // no fast-forward: still fresh

	if q, _ := mr.List(redisDecisionQueue); len(q) != 0 {
		t.Fatal("reclaimed a decision that was still being handled; it would run twice")
	}
}
