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
type decisionEnvelope struct {
	ID       string                  `json:"id"`
	Decision notify.CallbackDecision `json:"decision"`
	// Attempts counts deliveries already made. Carried in the payload
	// rather than a side table so it cannot be orphaned by a failed
	// cleanup.
	Attempts int `json:"attempts,omitempty"`
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
// KEYS: processing list, queue, started set. ARGV: stored member, replacement
// member ("" to drop it permanently).
var reclaimScript = redis.NewScript(`
local removed = redis.call('LREM', KEYS[1], 1, ARGV[1])
redis.call('ZREM', KEYS[3], ARGV[1])
if removed > 0 and ARGV[2] ~= '' then
  redis.call('LPUSH', KEYS[2], ARGV[2])
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
	data, err := json.Marshal(decisionEnvelope{ID: uuid.NewString(), Decision: d})
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
				// Unparseable: acking is the only way to stop it being
				// reclaimed forever.
				b.logger.WarnContext(ctx, "redis decision bus: unmarshal", "err", err)
				b.ack(raw)
				continue
			}

			select {
			case ch <- notify.Delivery{Decision: env.Decision, Ack: func() { b.ack(raw) }}:
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
		// Nothing can handle it, so redelivering it forever helps no one.
		b.logger.WarnContext(ctx, "redis decision bus: dropping unparseable in-flight decision", "err", err)
	} else if env.Attempts+1 >= maxDeliveryAttempts {
		b.logger.ErrorContext(ctx, "redis decision bus: giving up on a decision after repeated failures",
			"id", env.ID, "type", env.Decision.Type, "action", env.Decision.Action,
			"target_id", env.Decision.TargetID, "attempts", env.Attempts+1)
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
		[]string{redisDecisionProcessing, redisDecisionQueue, redisDecisionStarted},
		raw, replacement).Int64()
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
		return
	}
	b.logger.InfoContext(ctx, "redis decision bus: reclaimed an unacked decision",
		"id", env.ID, "attempt", env.Attempts)
}

func formatScore(f float64) string {
	return strconv.FormatFloat(f, 'f', 0, 64)
}
