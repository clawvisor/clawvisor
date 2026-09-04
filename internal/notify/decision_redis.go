package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

const (
	redisDecisionQueue      = "clawvisor:decisions"
	redisDecisionProcessing = "clawvisor:decisions:processing"
	// redisDecisionStarted scores each in-flight payload with the time it
	// was taken, so the reclaimer can tell a decision still being handled
	// from one whose handler died.
	redisDecisionStarted = "clawvisor:decisions:started"

	// visibilityTimeout is how long a taken decision may go unacked before
	// another instance may reclaim it. Comfortably longer than a handler,
	// since reclaiming early causes duplicate work rather than loss.
	visibilityTimeout = 2 * time.Minute
	reclaimInterval   = 30 * time.Second
)

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
	data, err := json.Marshal(d)
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
			// A failure here only costs a slower reclaim, never the
			// decision: it is already safe on the processing list.
			if err := b.rdb.ZAdd(ctx, redisDecisionStarted,
				redis.Z{Score: float64(time.Now().Unix()), Member: raw}).Err(); err != nil {
				b.logger.WarnContext(ctx, "redis decision bus: could not record start time", "err", err)
			}

			var d notify.CallbackDecision
			if err := json.Unmarshal([]byte(raw), &d); err != nil {
				// Unparseable: acking is the only way to stop it being
				// reclaimed forever.
				b.logger.WarnContext(ctx, "redis decision bus: unmarshal", "err", err)
				b.ack(raw)
				continue
			}

			select {
			case ch <- notify.Delivery{Decision: d, Ack: func() { b.ack(raw) }}:
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
		pipe := b.rdb.Pipeline()
		pipe.LRem(ctx, redisDecisionProcessing, 1, raw)
		pipe.LPush(ctx, redisDecisionQueue, raw)
		pipe.ZRem(ctx, redisDecisionStarted, raw)
		if _, err := pipe.Exec(ctx); err != nil {
			b.logger.WarnContext(ctx, "redis decision bus: reclaim failed", "err", err)
			continue
		}
		b.logger.InfoContext(ctx, "redis decision bus: reclaimed an unacked decision")
	}
}

func formatScore(f float64) string {
	return strconv.FormatFloat(f, 'f', 0, 64)
}
