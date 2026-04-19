package handlers

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisExtractionPendingPrefix = "clawvisor:extract:pending:"

// redisExtractionTracker is a Redis-backed ExtractionTracker for
// multi-instance deployments. Pending extractions are stored as a set per
// (taskID, sessionID) with a safety TTL so crashed instances don't orphan
// entries.
type redisExtractionTracker struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewRedisExtractionTracker creates a Redis-backed extraction tracker.
// ttl should exceed the extraction timeout + save timeout (i.e. > 40s).
func NewRedisExtractionTracker(rdb *redis.Client, ttl time.Duration) ExtractionTracker {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &redisExtractionTracker{rdb: rdb, ttl: ttl}
}

func (t *redisExtractionTracker) key(taskID, sessionID string) string {
	return redisExtractionPendingPrefix + taskID + ":" + sessionID
}

func (t *redisExtractionTracker) MarkPending(ctx context.Context, taskID, sessionID, auditID string) {
	if taskID == "" || sessionID == "" || auditID == "" {
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	key := t.key(taskID, sessionID)
	pipe := t.rdb.Pipeline()
	pipe.SAdd(opCtx, key, auditID)
	pipe.Expire(opCtx, key, t.ttl)
	_, _ = pipe.Exec(opCtx)
}

func (t *redisExtractionTracker) MarkDone(ctx context.Context, taskID, sessionID, auditID string) {
	if taskID == "" || sessionID == "" || auditID == "" {
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = t.rdb.SRem(opCtx, t.key(taskID, sessionID), auditID).Err()
}

func (t *redisExtractionTracker) HasPending(ctx context.Context, taskID, sessionID string) bool {
	if taskID == "" || sessionID == "" {
		return false
	}
	opCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, err := t.rdb.SCard(opCtx, t.key(taskID, sessionID)).Result()
	if err != nil {
		return false
	}
	return n > 0
}

var _ ExtractionTracker = (*memoryExtractionTracker)(nil)
var _ ExtractionTracker = (*redisExtractionTracker)(nil)
