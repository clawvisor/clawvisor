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
func (b *RedisDecisionBus) Subscribe(ctx context.Context) <-chan notify.CallbackDecision {
	ch := make(chan notify.CallbackDecision, 64)

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
				b.logger.Warn("redis decision bus: brpop", "err", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			// BRPop returns [key, value].
			if len(result) < 2 {
				continue
			}
			var d notify.CallbackDecision
			if err := json.Unmarshal([]byte(result[1]), &d); err != nil {
				b.logger.Warn("redis decision bus: unmarshal", "err", err)
				continue
			}
			select {
			case ch <- d:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch
}
