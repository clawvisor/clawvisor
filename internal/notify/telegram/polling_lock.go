package telegram

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisPollingLockPrefix = "clawvisor:tgpoll:"
	pollingLockTTL         = 35 * time.Second // slightly longer than getUpdates timeout
)

// PollingLock coordinates Telegram polling across instances. Only one
// instance should call getUpdates per bot token at a time.
type PollingLock interface {
	// Acquire tries to take the polling lock for the given user.
	// Returns true if this instance now holds the lock.
	Acquire(ctx context.Context, userID string) bool

	// Renew extends the lock TTL. Call periodically while polling.
	Renew(ctx context.Context, userID string)

	// Release gives up the lock.
	Release(ctx context.Context, userID string)
}

// noopPollingLock always acquires — used in single-instance mode.
type noopPollingLock struct{}

func (noopPollingLock) Acquire(context.Context, string) bool { return true }
func (noopPollingLock) Renew(context.Context, string)        {}
func (noopPollingLock) Release(context.Context, string)      {}

// redisPollingLock uses SET NX with TTL for distributed mutual exclusion.
type redisPollingLock struct {
	rdb      *redis.Client
	instanceID string
}

// NewRedisPollingLock creates a Redis-backed distributed polling lock.
func NewRedisPollingLock(rdb *redis.Client, instanceID string) PollingLock {
	return &redisPollingLock{rdb: rdb, instanceID: instanceID}
}

func (l *redisPollingLock) Acquire(ctx context.Context, userID string) bool {
	ok, err := l.rdb.SetNX(ctx, redisPollingLockPrefix+userID, l.instanceID, pollingLockTTL).Result()
	if err != nil {
		return false
	}
	return ok
}

func (l *redisPollingLock) Renew(ctx context.Context, userID string) {
	// Only renew if we still own the lock.
	script := redis.NewScript(`
		if redis.call('GET', KEYS[1]) == ARGV[1] then
			return redis.call('PEXPIRE', KEYS[1], ARGV[2])
		end
		return 0
	`)
	_, _ = script.Run(ctx, l.rdb, []string{redisPollingLockPrefix + userID},
		l.instanceID, int(pollingLockTTL.Milliseconds())).Result()
}

func (l *redisPollingLock) Release(ctx context.Context, userID string) {
	// Only delete if we own the lock.
	script := redis.NewScript(`
		if redis.call('GET', KEYS[1]) == ARGV[1] then
			return redis.call('DEL', KEYS[1])
		end
		return 0
	`)
	_, _ = script.Run(ctx, l.rdb, []string{redisPollingLockPrefix + userID}, l.instanceID).Result()
}
