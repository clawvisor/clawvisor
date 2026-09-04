package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

const (
	redisDecisionQueue      = "clawvisor:decisions"
	redisDecisionProcessing = "clawvisor:decisions:processing"
	// redisDecisionStarted scores each in-flight delivery with the time it
	// was taken, so the reclaimer can tell a decision still being handled
	// from one whose handler died.
	redisDecisionStarted = "clawvisor:decisions:started"
	// redisDecisionDeadLetter holds deliveries that could not be handled.
	// Retiring one destroys a human's decision, so they are kept here for
	// inspection and manual replay instead of being deleted.
	redisDecisionDeadLetter = "clawvisor:decisions:dead"
	// deadLetterMaxLen bounds that list; it should normally be empty, and an
	// unbounded one would grow forever if something started failing in a loop.
	deadLetterMaxLen = 1000

	// visibilityTimeout is how long a taken decision may go unacked before
	// another instance may reclaim it. Comfortably longer than a handler,
	// since reclaiming early causes duplicate work rather than loss.
	visibilityTimeout = 2 * time.Minute
	reclaimInterval   = 30 * time.Second

	// maxDeliveryAttempts caps redelivery of a decision that keeps failing.
	// Handler errors do not ack, so without a cap a permanently failing
	// decision — an approval whose row is long gone — would be retried
	// forever. Five attempts at the visibility timeout is roughly ten
	// minutes of trying before giving up loudly.
	maxDeliveryAttempts = 5
)

// decisionEnvelope wraps a decision with a per-delivery identity.
//
// The ID exists because the queue entry, the processing entry and the
// visibility marker all have to refer to the same delivery, and the decision
// payload alone cannot do that: two publishes of the same decision marshal
// to identical bytes, so they would share one marker in the started set. The
// first ack would remove that shared marker and strand the other delivery on
// the processing list, unreclaimable — lost exactly like the design this
// replaced. A UUID per publish makes every entry distinct.
//
// The decision is embedded rather than nested so the envelope stays wire
// compatible with the flat payload this replaces, in both directions. A
// rolling deploy runs both formats against one queue: an old instance
// decoding this envelope still sees every decision field and ignores the
// underscored ones, and a new instance decoding a flat payload fills the
// embedded struct and simply has no ID. Nested under a "decision" key,
// neither could read the other's payload — it would unmarshal cleanly into a
// zero-valued decision, match no case in the consumer, and be acked as
// handled. That is a silent loss of exactly the kind this bus exists to
// prevent, and a mixed fleet is guaranteed during every deploy.
type decisionEnvelope struct {
	notify.CallbackDecision
	ID string `json:"_delivery_id,omitempty"`
	// Attempts counts deliveries already made. Carried in the payload
	// rather than a side table so it cannot be orphaned by a failed
	// cleanup.
	Attempts int `json:"_attempts,omitempty"`
}

// reclaimScript returns one stale delivery to the queue.
//
// LREM and LPUSH have to be atomic: between a reclaimer reading a marker and
// requeuing it, the original handler may finish and ack. Split into separate
// commands the LREM then removes nothing while the LPUSH still runs, and an
// already-acknowledged decision is handled a second time. Gating the push on
// the removal makes the ack and the reclaim mutually exclusive — whichever
// gets there first wins, and the loser is a no-op.
//
// A delivery that is out of attempts is moved to the dead-letter list rather
// than deleted, in the same atomic step, so giving up never destroys it.
//
// KEYS: processing list, queue, started set, dead-letter list. ARGV: stored
// member, replacement member ("" to dead-letter instead), dead-letter bound.
var reclaimScript = redis.NewScript(`
local removed = redis.call('LREM', KEYS[1], 1, ARGV[1])
redis.call('ZREM', KEYS[3], ARGV[1])
if removed > 0 then
  if ARGV[2] ~= '' then
    redis.call('LPUSH', KEYS[2], ARGV[2])
  else
    redis.call('LPUSH', KEYS[4], ARGV[1])
    redis.call('LTRIM', KEYS[4], 0, ARGV[3])
  end
end
return removed
`)

// RedisDecisionBus distributes callback decisions across instances.
//
// Decisions are moved, not popped: BLMOVE takes a payload from the queue and
// places it on a processing list in one atomic step, so a decision is never
// only in memory. Ack removes it from processing; anything left there past
// the visibility timeout is returned to the queue by the reclaimer.
//
// This replaces a BRPOP design that lost approvals. BRPOP is a destructive
// read, so between the pop and a finished handler the sole copy lived in a
// goroutine, and a scale-down or rolling deploy dropped it silently — on
// staging, roughly half of all approvals. No shutdown ordering can close
// that, because there is no instant at which the decision is both off the
// queue and durably owned; an acknowledgement boundary is the property that
// was missing.
type RedisDecisionBus struct {
	rdb    *redis.Client
	logger *slog.Logger
}

// NewRedisDecisionBus creates a Redis-backed decision bus.
func NewRedisDecisionBus(rdb *redis.Client, logger *slog.Logger) *RedisDecisionBus {
	return &RedisDecisionBus{rdb: rdb, logger: logger}
}

// Publish enqueues a decision for whichever instance takes it next.
func (b *RedisDecisionBus) Publish(ctx context.Context, d notify.CallbackDecision) error {
	data, err := json.Marshal(decisionEnvelope{CallbackDecision: d, ID: uuid.NewString()})
	if err != nil {
		return err
	}
	return b.rdb.LPush(ctx, redisDecisionQueue, data).Err()
}

// Subscribe returns a channel of deliveries taken from the shared queue.
// Every instance polls the same queue, so exactly one takes each decision —
// and it stays recoverable until that instance acks.
func (b *RedisDecisionBus) Subscribe(ctx context.Context) <-chan notify.Delivery {
	ch := make(chan notify.Delivery)

	go b.reclaimLoop(ctx)

	go func() {
		defer close(ch)

		for {
			if ctx.Err() != nil {
				return
			}

			raw, err := b.rdb.BLMove(ctx, redisDecisionQueue, redisDecisionProcessing,
				"RIGHT", "LEFT", 1*time.Second).Result()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if err == redis.Nil {
					continue // idle, poll again
				}
				b.logger.WarnContext(ctx, "redis decision bus: blmove", "err", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}

			// Record when this was taken so the reclaimer can age it out.
			// If this fails the entry is still recoverable: the reclaimer
			// adopts processing entries that have no marker rather than
			// ignoring them, so the worst case is a later reclaim, not a
			// lost decision.
			if err := b.rdb.ZAdd(ctx, redisDecisionStarted,
				redis.Z{Score: float64(time.Now().Unix()), Member: raw}).Err(); err != nil {
				b.logger.WarnContext(ctx, "redis decision bus: could not record start time", "err", err)
			}

			var env decisionEnvelope
			if err := json.Unmarshal([]byte(raw), &env); err != nil {
				// Set aside rather than acked: acking would delete a
				// payload nobody has looked at, and this build being
				// unable to read it does not make it worthless.
				b.logger.WarnContext(ctx, "redis decision bus: unmarshal", "err", err)
				b.deadLetter(raw)
				continue
			}
			if env.Type == "" {
				// Decodes, but names no decision — a payload from a
				// format this build does not know. Delivering it would
				// match no case in the consumer, which would then ack it
				// as handled and destroy it silently.
				b.logger.WarnContext(ctx, "redis decision bus: payload carries no decision type")
				b.deadLetter(raw)
				continue
			}

			select {
			case ch <- notify.Delivery{Decision: env.CallbackDecision, Ack: func() { b.ack(raw) }}:
			case <-ctx.Done():
				// Deliberately not acked. The decision stays on the
				// processing list and the reclaimer returns it, which is
				// exactly the case the old design lost.
				return
			}
		}
	}()

	return ch
}

// ack retires a handled decision.
func (b *RedisDecisionBus) ack(raw string) {
	// Detached: the common caller is a handler finishing as the process
	// shuts down, and reusing a cancelled context would leave the decision
	// to be reclaimed and handled twice.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pipe := b.rdb.Pipeline()
	pipe.LRem(ctx, redisDecisionProcessing, 1, raw)
	pipe.ZRem(ctx, redisDecisionStarted, raw)
	if _, err := pipe.Exec(ctx); err != nil {
		// Not lost, just duplicated later: the reclaimer will return it
		// and a handler will run again. Idempotent handling is what makes
		// that safe.
		b.logger.WarnContext(ctx, "redis decision bus: ack failed, decision will be redelivered", "err", err)
	}
}

// deadLetter sets aside a payload this build cannot act on.
//
// Deliberately not an ack: an ack deletes, and a decision a human made is
// not something to delete because the process could not read it. Keeping it
// on a bounded list costs nothing and leaves it recoverable.
func (b *RedisDecisionBus) deadLetter(raw string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pipe := b.rdb.Pipeline()
	pipe.LRem(ctx, redisDecisionProcessing, 1, raw)
	pipe.ZRem(ctx, redisDecisionStarted, raw)
	pipe.LPush(ctx, redisDecisionDeadLetter, raw)
	pipe.LTrim(ctx, redisDecisionDeadLetter, 0, deadLetterMaxLen-1)
	if _, err := pipe.Exec(ctx); err != nil {
		b.logger.WarnContext(ctx, "redis decision bus: could not dead-letter a decision", "err", err)
	}
}

// reclaimLoop returns decisions whose handler never acked.
func (b *RedisDecisionBus) reclaimLoop(ctx context.Context) {
	t := time.NewTicker(reclaimInterval)
	defer t.Stop()

	b.reclaim(ctx) // once at startup, for anything a previous instance left
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.reclaim(ctx)
		}
	}
}

func (b *RedisDecisionBus) reclaim(ctx context.Context) {
	b.adoptUnmarked(ctx)

	cutoff := float64(time.Now().Add(-visibilityTimeout).Unix())
	stale, err := b.rdb.ZRangeByScore(ctx, redisDecisionStarted, &redis.ZRangeBy{
		Min: "-inf", Max: formatScore(cutoff),
	}).Result()
	if err != nil {
		if ctx.Err() == nil {
			b.logger.WarnContext(ctx, "redis decision bus: reclaim scan failed", "err", err)
		}
		return
	}

	for _, raw := range stale {
		b.requeue(ctx, raw)
	}
}

// adoptUnmarked gives a start time to processing entries that have none.
//
// BLMOVE and the ZADD that follows it are two commands, so a crash or a
// cancelled context between them leaves an entry on the processing list that
// the reclaimer's scan would never see — the one window in which this design
// could still lose a decision permanently. Rather than making those two
// commands atomic, the reclaimer treats an unmarked processing entry as one
// it has just noticed: it gets a marker now and ages out normally. Marked NX
// so a real start time recorded concurrently is never overwritten, which
// would otherwise extend a live handler's visibility window.
func (b *RedisDecisionBus) adoptUnmarked(ctx context.Context) {
	inflight, err := b.rdb.LRange(ctx, redisDecisionProcessing, 0, -1).Result()
	if err != nil {
		if ctx.Err() == nil {
			b.logger.WarnContext(ctx, "redis decision bus: processing scan failed", "err", err)
		}
		return
	}
	if len(inflight) == 0 {
		return
	}

	marked, err := b.rdb.ZRange(ctx, redisDecisionStarted, 0, -1).Result()
	if err != nil {
		if ctx.Err() == nil {
			b.logger.WarnContext(ctx, "redis decision bus: marker scan failed", "err", err)
		}
		return
	}
	have := make(map[string]struct{}, len(marked))
	for _, m := range marked {
		have[m] = struct{}{}
	}

	now := float64(time.Now().Unix())
	for _, raw := range inflight {
		if _, ok := have[raw]; ok {
			continue
		}
		if err := b.rdb.ZAddNX(ctx, redisDecisionStarted,
			redis.Z{Score: now, Member: raw}).Err(); err != nil {
			b.logger.WarnContext(ctx, "redis decision bus: could not adopt unmarked decision", "err", err)
			continue
		}
		b.logger.InfoContext(ctx, "redis decision bus: adopted an unmarked in-flight decision")
	}
}

// requeue returns one stale delivery to the queue, or drops it if it has been
// retried too many times.
func (b *RedisDecisionBus) requeue(ctx context.Context, raw string) {
	var env decisionEnvelope
	replacement := ""
	drop := true

	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		// Redelivering something nothing can parse helps no one, but it
		// still goes to the dead-letter list rather than being deleted.
		b.logger.WarnContext(ctx, "redis decision bus: dead-lettering unparseable in-flight decision", "err", err)
	} else if env.Attempts+1 >= maxDeliveryAttempts {
		// Out of attempts. Handler errors cannot tell a permanently
		// unhandleable decision from a long outage, so this is not
		// allowed to destroy it: it moves to the dead-letter list, where
		// it can be inspected and replayed.
		b.logger.ErrorContext(ctx, "redis decision bus: dead-lettering a decision after repeated failures",
			"id", env.ID, "type", env.Type, "action", env.Action,
			"target_id", env.TargetID, "attempts", env.Attempts+1)
	} else {
		env.Attempts++
		next, err := json.Marshal(env)
		if err != nil {
			b.logger.WarnContext(ctx, "redis decision bus: could not re-encode decision", "err", err)
			return // leave it in place; a later pass can try again
		}
		replacement = string(next)
		drop = false
	}

	removed, err := reclaimScript.Run(ctx, b.rdb,
		[]string{redisDecisionProcessing, redisDecisionQueue, redisDecisionStarted, redisDecisionDeadLetter},
		raw, replacement, deadLetterMaxLen-1).Int64()
	if err != nil {
		if ctx.Err() == nil {
			b.logger.WarnContext(ctx, "redis decision bus: reclaim failed", "err", err)
		}
		return
	}
	if removed == 0 {
		// Acked while this pass was running, or a marker left behind by an
		// entry that is already gone. Either way the script cleared the
		// marker and deliberately did not requeue.
		return
	}
	if drop {
		return // already logged, and now on the dead-letter list
	}
	b.logger.InfoContext(ctx, "redis decision bus: reclaimed an unacked decision",
		"id", env.ID, "attempt", env.Attempts)
}

func formatScore(f float64) string {
	return strconv.FormatFloat(f, 'f', 0, 64)
}
