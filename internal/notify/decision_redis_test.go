package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
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

// BLMOVE and the ZADD that marks the entry in flight are two commands. A
// crash or cancelled context between them left an entry on the processing
// list that the reclaim scan never looked at, so it was never redelivered —
// the one window in which this design could still lose a decision outright.
func TestDecisionBus_UnmarkedInFlightDecisionIsRecovered(t *testing.T) {
	bus, mr := newTestBus(t)
	publish(t, bus, "task-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := bus.Subscribe(ctx)

	select {
	case <-ch: // taken, never acked
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery")
	}

	inflight, _ := mr.List(redisDecisionProcessing)
	if len(inflight) != 1 {
		t.Fatalf("expected one in-flight decision, got %d", len(inflight))
	}
	// Exactly the state a failed ZADD leaves behind: in flight, unmarked.
	if err := bus.rdb.ZRem(context.Background(), redisDecisionStarted, inflight[0]).Err(); err != nil {
		t.Fatal(err)
	}

	bus.reclaim(context.Background()) // must notice it and start its clock

	if err := bus.rdb.ZScore(context.Background(), redisDecisionStarted, inflight[0]).Err(); err != nil {
		t.Fatalf("an unmarked in-flight decision was never adopted; it is unreclaimable: %v", err)
	}

	// Age it out and confirm it actually comes back.
	if err := bus.rdb.ZAdd(context.Background(), redisDecisionStarted,
		redis.Z{Score: float64(time.Now().Add(-2 * visibilityTimeout).Unix()), Member: inflight[0]}).Err(); err != nil {
		t.Fatal(err)
	}
	bus.reclaim(context.Background())

	select {
	case d := <-ch:
		if d.Decision.TargetID != "task-1" {
			t.Fatalf("redelivered the wrong decision: %+v", d.Decision)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an unmarked in-flight decision was never redelivered; it is lost")
	}
}

// Two publishes of the same decision are two deliveries. Keyed on the payload
// they collided: one marker for two processing entries, so the first ack
// removed the shared marker and stranded the second with nothing to reclaim
// it.
func TestDecisionBus_IdenticalDecisionsDoNotCollide(t *testing.T) {
	bus, mr := newTestBus(t)
	publish(t, bus, "task-1")
	publish(t, bus, "task-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := bus.Subscribe(ctx)

	var first notify.Delivery
	for i := 0; i < 2; i++ {
		select {
		case d := <-ch:
			if i == 0 {
				first = d
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("only got %d of 2 deliveries", i)
		}
	}

	inflight, _ := mr.List(redisDecisionProcessing)
	if len(inflight) != 2 {
		t.Fatalf("expected two in-flight decisions, got %d", len(inflight))
	}

	first.Ack()

	// Acking one must leave the other both in flight and still tracked.
	deadline := time.Now().Add(2 * time.Second)
	for {
		remaining, _ := mr.List(redisDecisionProcessing)
		if len(remaining) == 1 {
			marked, err := bus.rdb.ZRange(context.Background(), redisDecisionStarted, 0, -1).Result()
			if err != nil {
				t.Fatal(err)
			}
			if len(marked) != 1 || marked[0] != remaining[0] {
				t.Fatalf("acking one delivery destroyed the other's marker; it is unreclaimable (markers: %d)", len(marked))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ack did not retire exactly one delivery (%d left in flight)", len(remaining))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// And it is genuinely recoverable, not merely marked.
	remaining, _ := mr.List(redisDecisionProcessing)
	if err := bus.rdb.ZAdd(context.Background(), redisDecisionStarted,
		redis.Z{Score: float64(time.Now().Add(-2 * visibilityTimeout).Unix()), Member: remaining[0]}).Err(); err != nil {
		t.Fatal(err)
	}
	bus.reclaim(context.Background())

	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("the second identical decision was never redelivered; it is lost")
	}
}

// An ack landing between a reclaim's scan and its requeue leaves a marker
// with no processing entry behind it. Requeuing unconditionally then runs an
// already-handled decision a second time.
func TestDecisionBus_StaleMarkerDoesNotResurrectAnAckedDecision(t *testing.T) {
	bus, mr := newTestBus(t)
	ctx := context.Background()

	raw := `{"Type":"task","Action":"approve","TargetID":"task-1","_delivery_id":"already-acked"}`
	if err := bus.rdb.ZAdd(ctx, redisDecisionStarted, redis.Z{
		Score: float64(time.Now().Add(-2 * visibilityTimeout).Unix()), Member: raw,
	}).Err(); err != nil {
		t.Fatal(err)
	}

	bus.reclaim(ctx)

	if q, _ := mr.List(redisDecisionQueue); len(q) != 0 {
		t.Fatal("requeued a decision that was no longer in flight; an acked decision would be handled twice")
	}
	marked, err := bus.rdb.ZRange(ctx, redisDecisionStarted, 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) != 0 {
		t.Fatalf("stale marker survived reclaim and will be rescanned forever (%d left)", len(marked))
	}
}

// Handler errors no longer ack, so redelivery has to be bounded: a decision
// whose row is long gone would otherwise be retried for ever.
func TestDecisionBus_GivesUpAfterRepeatedRedelivery(t *testing.T) {
	bus, mr := newTestBus(t)
	publish(t, bus, "task-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := bus.Subscribe(ctx)

	deliveries := 0
	for i := 0; i < maxDeliveryAttempts+3; i++ {
		select {
		case <-ch: // never acked, as a failing handler never would
			deliveries++
		case <-time.After(2 * time.Second):
			i = maxDeliveryAttempts + 3 // no more deliveries coming
			continue
		}

		inflight, _ := mr.List(redisDecisionProcessing)
		if len(inflight) == 0 {
			break
		}
		if err := bus.rdb.ZAdd(context.Background(), redisDecisionStarted,
			redis.Z{Score: float64(time.Now().Add(-2 * visibilityTimeout).Unix()), Member: inflight[0]}).Err(); err != nil {
			t.Fatal(err)
		}
		bus.reclaim(context.Background())
	}

	if deliveries != maxDeliveryAttempts {
		t.Fatalf("delivered %d times, want the %d-attempt cap: an unhandleable decision retries forever",
			deliveries, maxDeliveryAttempts)
	}
	if q, _ := mr.List(redisDecisionQueue); len(q) != 0 {
		t.Fatalf("a decision past its attempt cap is still queued (%d)", len(q))
	}
	if p, _ := mr.List(redisDecisionProcessing); len(p) != 0 {
		t.Fatalf("a decision past its attempt cap is still in flight (%d)", len(p))
	}
	// Given up on, but not destroyed: a human made this decision, and
	// handler errors cannot prove it was unhandleable rather than unlucky.
	dead, _ := mr.List(redisDecisionDeadLetter)
	if len(dead) != 1 {
		t.Fatalf("a decision past its attempt cap was deleted rather than dead-lettered (%d held)", len(dead))
	}
	if !strings.Contains(dead[0], "task-1") {
		t.Fatalf("dead-lettered the wrong payload: %s", dead[0])
	}
}

// A rolling deploy runs both payload formats against one queue. A decision
// published by an instance on the previous flat format has to survive being
// taken by an instance on this one: nested under a "decision" key it would
// unmarshal into a zero-valued decision, match no case in the consumer, and
// be acked as handled — losing it silently.
func TestDecisionBus_LegacyFlatPayloadIsStillDelivered(t *testing.T) {
	bus, _ := newTestBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Exactly what the previous build wrote: a bare CallbackDecision.
	legacy, err := json.Marshal(notify.CallbackDecision{
		Type: "task", Action: "approve", TargetID: "task-legacy", UserID: "user-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.rdb.LPush(ctx, redisDecisionQueue, legacy).Err(); err != nil {
		t.Fatal(err)
	}

	ch := bus.Subscribe(ctx)
	select {
	case d := <-ch:
		if d.Decision.TargetID != "task-legacy" || d.Decision.Action != "approve" {
			t.Fatalf("a decision published before this format was lost in translation: %+v", d.Decision)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a decision from the previous payload format was never delivered")
	}
}

// The other direction of the same deploy: an instance still on the previous
// build decodes what this one publishes. The envelope embeds the decision so
// its fields stay at the top level, where a plain CallbackDecision decoder
// still finds them.
func TestDecisionBus_PublishedPayloadIsReadableByThePreviousFormat(t *testing.T) {
	bus, mr := newTestBus(t)
	publish(t, bus, "task-1")

	queued, _ := mr.List(redisDecisionQueue)
	if len(queued) != 1 {
		t.Fatalf("expected one queued decision, got %d", len(queued))
	}

	var asOldBuildWouldSeeIt notify.CallbackDecision
	if err := json.Unmarshal([]byte(queued[0]), &asOldBuildWouldSeeIt); err != nil {
		t.Fatalf("an instance on the previous build cannot parse this payload: %v", err)
	}
	if asOldBuildWouldSeeIt.TargetID != "task-1" || asOldBuildWouldSeeIt.Action != "approve" {
		t.Fatalf("an instance on the previous build would read an empty decision and ack it away: %+v",
			asOldBuildWouldSeeIt)
	}
}

// A payload naming no decision must not be delivered: the consumer matches no
// case, treats it as handled, and acks it out of existence.
func TestDecisionBus_UnrecognisedPayloadIsDeadLetteredNotAcked(t *testing.T) {
	bus, mr := newTestBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bus.rdb.LPush(ctx, redisDecisionQueue, `{"something":"else"}`).Err(); err != nil {
		t.Fatal(err)
	}
	ch := bus.Subscribe(ctx)

	select {
	case d := <-ch:
		t.Fatalf("delivered a payload carrying no decision; the consumer would ack it away: %+v", d.Decision)
	case <-time.After(1500 * time.Millisecond):
	}

	dead, _ := mr.List(redisDecisionDeadLetter)
	if len(dead) != 1 {
		t.Fatalf("an unreadable payload was deleted rather than kept for inspection (%d held)", len(dead))
	}
}
