package handlers

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisDevicePairingPrefix = "clawvisor:devicepairing:"
	redisDevicePairingLock   = "clawvisor:devicepairing:lock:"
	pairingLockTTL           = 10 * time.Second
	pairingLockRetryDelay    = 50 * time.Millisecond
	pairingLockMaxRetries    = 20 // 20 × 50ms = 1s max wait
)

// RedisDevicePairingStore stores device pairing sessions in Redis.
type RedisDevicePairingStore struct {
	rdb *redis.Client
}

// NewRedisDevicePairingStore creates a Redis-backed device pairing store.
func NewRedisDevicePairingStore(rdb *redis.Client) *RedisDevicePairingStore {
	return &RedisDevicePairingStore{rdb: rdb}
}

func (s *RedisDevicePairingStore) Store(token string, session *pairingSession) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := marshalPairingSession(session)
	if err != nil {
		return
	}
	ttl := time.Until(session.ExpiresAt)
	if ttl <= 0 {
		ttl = pairingExpiry
	}
	_ = s.rdb.Set(ctx, redisDevicePairingPrefix+token, data, ttl).Err()
}

func (s *RedisDevicePairingStore) LoadAndLock(token string) (*pairingSession, bool, func(bool)) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Acquire a distributed lock for this token to prevent concurrent
	// CompletePairing requests from racing past the attempt limit.
	lockKey := redisDevicePairingLock + token
	acquired := false
	for i := 0; i < pairingLockMaxRetries; i++ {
		ok, err := s.rdb.SetNX(ctx, lockKey, "1", pairingLockTTL).Result()
		if err != nil {
			return nil, false, nil
		}
		if ok {
			acquired = true
			break
		}
		time.Sleep(pairingLockRetryDelay)
	}
	if !acquired {
		return nil, false, nil
	}

	data, err := s.rdb.Get(ctx, redisDevicePairingPrefix+token).Bytes()
	if err != nil {
		// Release lock if session not found.
		_ = s.rdb.Del(ctx, lockKey).Err()
		return nil, false, nil
	}
	session, err := unmarshalPairingSession(data)
	if err != nil {
		_ = s.rdb.Del(ctx, lockKey).Err()
		return nil, false, nil
	}

	done := func(del bool) {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		// Always release the lock when done.
		defer s.rdb.Del(dctx, lockKey)
		if del {
			_ = s.rdb.Del(dctx, redisDevicePairingPrefix+token).Err()
		} else {
			// Write back updated session (e.g. incremented attempts).
			updated, err := marshalPairingSession(session)
			if err == nil {
				ttl := time.Until(session.ExpiresAt)
				if ttl <= 0 {
					_ = s.rdb.Del(dctx, redisDevicePairingPrefix+token).Err()
				} else {
					_ = s.rdb.Set(dctx, redisDevicePairingPrefix+token, updated, ttl).Err()
				}
			}
		}
	}
	return session, true, done
}

// RunCleanup is a no-op — Redis TTL handles expiry automatically.
func (s *RedisDevicePairingStore) RunCleanup(done <-chan struct{}) {}

var _ DevicePairingStore = (*RedisDevicePairingStore)(nil)
var _ DevicePairingStore = (*memoryDevicePairingStore)(nil)
