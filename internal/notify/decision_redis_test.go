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

// The subscriber channel must stay unbuffered. A buffer here is not a
// throughput win, it is a loss window: BRPOP is a destructive read, so a
// decision sitting in a buffer when the instance drains is already gone from
// Redis and will never be delivered. Unbuffered means a decision leaves the
// queue only when a consumer is ready to take it, which is what took staging
// from losing roughly half of all approvals to losing none.
func TestRedisDecisionBus_DoesNotPrefetchIntoABuffer(t *testing.T) {
	bus, mr := newTestBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 3; i++ {
		if err := bus.Publish(ctx, notify.CallbackDecision{
			Type: "task", Action: "approve", TargetID: "task-1",
		}); err != nil {
			t.Fatal(err)
		}
	}

	ch := bus.Subscribe(ctx)

	// Take one and let the loop settle. A buffered channel would have
	// drained the queue entirely; unbuffered leaves the rest in Redis.
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("no decision delivered")
	}
	time.Sleep(200 * time.Millisecond)

	remaining, _ := mr.List(redisDecisionQueue)
	if len(remaining) == 0 {
		t.Fatal("the queue was drained into memory; those decisions would be lost if the instance stopped")
	}
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
