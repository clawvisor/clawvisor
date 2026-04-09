package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/redis/go-redis/v9"
)

// RedisHub implements EventHub using Redis pub/sub for cross-instance event
// delivery, with a local in-memory Hub for per-connection fan-out within
// a single process.
type RedisHub struct {
	rdb    *redis.Client
	local  *Hub
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	// Per-user Redis subscription ref-counting. When the first local subscriber
	// for a user appears, we create a Redis subscription; when the last
	// unsubscribes, we tear it down.
	mu    sync.Mutex
	subs  map[string]*userSub // userID → subscription state
}

type userSub struct {
	cancel context.CancelFunc
	refs   int
}

// NewRedisHub creates a hub backed by Redis pub/sub.
func NewRedisHub(ctx context.Context, rdb *redis.Client, logger *slog.Logger) *RedisHub {
	ctx, cancel := context.WithCancel(ctx)
	return &RedisHub{
		rdb:    rdb,
		local:  NewHub(),
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
		subs:   make(map[string]*userSub),
	}
}

func redisChannel(userID string) string {
	return "clawvisor:events:" + userID
}

// Publish sends an event to all subscribers across all instances.
func (h *RedisHub) Publish(userID string, e Event) {
	data, err := json.Marshal(e)
	if err != nil {
		h.logger.Warn("redis hub: marshal event", "err", err)
		return
	}
	if err := h.rdb.Publish(h.ctx, redisChannel(userID), data).Err(); err != nil {
		h.logger.Warn("redis hub: publish", "err", err, "user_id", userID)
	}
}

// Subscribe returns a channel that receives events for the given user.
// Events published on any instance are delivered. The returned unsub
// function must be called when the subscriber is done.
func (h *RedisHub) Subscribe(userID string) (<-chan Event, func()) {
	ch, localUnsub := h.local.Subscribe(userID)

	h.mu.Lock()
	us, exists := h.subs[userID]
	if !exists {
		subCtx, subCancel := context.WithCancel(h.ctx)
		us = &userSub{cancel: subCancel}
		h.subs[userID] = us
		go h.forwardFromRedis(subCtx, userID)
	}
	us.refs++
	h.mu.Unlock()

	unsub := func() {
		localUnsub()

		h.mu.Lock()
		defer h.mu.Unlock()
		if us, ok := h.subs[userID]; ok {
			us.refs--
			if us.refs <= 0 {
				us.cancel()
				delete(h.subs, userID)
			}
		}
	}

	return ch, unsub
}

// forwardFromRedis subscribes to the Redis channel for a user and forwards
// received events to the local hub.
func (h *RedisHub) forwardFromRedis(ctx context.Context, userID string) {
	pubsub := h.rdb.Subscribe(ctx, redisChannel(userID))
	defer pubsub.Close()

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var e Event
			if err := json.Unmarshal([]byte(msg.Payload), &e); err != nil {
				h.logger.Warn("redis hub: unmarshal event", "err", err)
				continue
			}
			h.local.Publish(userID, e)
		}
	}
}

// Close tears down all Redis subscriptions and closes the local hub.
func (h *RedisHub) Close() {
	h.cancel()
	h.local.Close()
}
