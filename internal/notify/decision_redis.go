package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

const redisDecisionQueue = "clawvisor:decisions"

// RedisDecisionBus implements notify.DecisionBus using a Redis list for
// exactly-once delivery. Only one instance receives each decision, avoiding
// duplicate side effects (callback webhooks, state transitions).
type RedisDecisionBus struct {
	rdb    *redis.Client
	logger *slog.Logger
}

// NewRedisDecisionBus creates a Redis-backed decision bus.
func NewRedisDecisionBus(rdb *redis.Client, logger *slog.Logger) *RedisDecisionBus {
	return &RedisDecisionBus{rdb: rdb, logger: logger}
}

// Publish pushes a decision onto the Redis list (LPUSH).
func (b *RedisDecisionBus) Publish(ctx context.Context, d notify.CallbackDecision) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return b.rdb.LPush(ctx, redisDecisionQueue, data).Err()
}

// Subscribe returns a channel that receives decisions. Uses BRPOP for
// blocking, exactly-once consumption — only one instance processes each
// decision even when multiple instances are subscribed.
// requeue returns an already-popped decision to the queue.
//
// Uses a fresh context: the only caller runs because the subscription's
// context was cancelled, so reusing it would fail the write for the same
// reason it is needed.
func (b *RedisDecisionBus) requeue(raw string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.rdb.RPush(ctx, redisDecisionQueue, raw).Err(); err != nil {
		// Nothing further to try: the decision is lost, and saying so is
		// the only thing that separates this from a silent drop.
		b.logger.ErrorContext(ctx, "redis decision bus: lost a decision on shutdown", "err", err)
		return
	}
	b.logger.InfoContext(ctx, "redis decision bus: returned a decision to the queue on shutdown")
}

func (b *RedisDecisionBus) Subscribe(ctx context.Context) <-chan notify.CallbackDecision {
	// Unbuffered on purpose. A buffer here is not a throughput win, it is a
	// loss window: BRPOP is a destructive read, so anything sitting in the
	// buffer when the instance drains is gone from Redis and never
	// delivered. Unbuffered means a decision only leaves the queue when a
	// consumer is ready to take it, so a cancelled context finds the send
	// still blocked and can put it back.
	ch := make(chan notify.CallbackDecision)

	go func() {
		defer close(ch)

		for {
			// BRPOP blocks for up to 1s, then loops to check ctx cancellation.
			result, err := b.rdb.BRPop(ctx, 1*time.Second, redisDecisionQueue).Result()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if err == redis.Nil {
					continue // timeout, loop again
				}
				b.logger.WarnContext(ctx, "redis decision bus: brpop", "err", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			// BRPop returns [key, value].
			if len(result) < 2 {
				continue
			}
			var d notify.CallbackDecision
			if err := json.Unmarshal([]byte(result[1]), &d); err != nil {
				b.logger.WarnContext(ctx, "redis decision bus: unmarshal", "err", err)
				continue
			}
			select {
			case ch <- d:
			case <-ctx.Done():
				// BRPOP is a destructive read, so this decision now exists
				// only in this goroutine. Returning here would drop a
				// human's approval with no error, no log and no
				// redelivery — and a shutting-down instance keeps popping
				// right up until its context is cancelled, so this is the
				// common path during a rolling deploy, not a rare one.
				//
				// Put it back on the tail, which is where BRPOP takes
				// from, so the next instance to poll picks it up.
				b.requeue(result[1])
				return
			}
		}
	}()

	return ch
}
