package handlers

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisPairingCodeKey     = "clawvisor:pairingcode:code"
	redisPairingAttemptsKey = "clawvisor:pairingcode:attempts"
)

// RedisPairingCodeStore stores the MCP pairing code in Redis so generation
// and verification can happen on different instances.
type RedisPairingCodeStore struct {
	rdb         *redis.Client
	expiry      time.Duration
	maxAttempts int
}

// NewRedisPairingCodeStore creates a Redis-backed pairing code store.
func NewRedisPairingCodeStore(rdb *redis.Client, expiry time.Duration, maxAttempts int) *RedisPairingCodeStore {
	return &RedisPairingCodeStore{rdb: rdb, expiry: expiry, maxAttempts: maxAttempts}
}

func (s *RedisPairingCodeStore) Set(code string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pipe := s.rdb.Pipeline()
	pipe.Set(ctx, redisPairingCodeKey, code, s.expiry)
	pipe.Set(ctx, redisPairingAttemptsKey, "0", s.expiry)
	_, _ = pipe.Exec(ctx)
}

func (s *RedisPairingCodeStore) Verify(code string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Use a Lua script for atomic check-and-consume.
	script := redis.NewScript(`
		local stored = redis.call('GET', KEYS[1])
		if not stored then return 0 end
		if stored == ARGV[1] then
			redis.call('DEL', KEYS[1])
			redis.call('DEL', KEYS[2])
			return 1
		end
		local attempts = redis.call('INCR', KEYS[2])
		if tonumber(attempts) >= tonumber(ARGV[2]) then
			redis.call('DEL', KEYS[1])
			redis.call('DEL', KEYS[2])
		end
		return 0
	`)

	result, err := script.Run(ctx, s.rdb,
		[]string{redisPairingCodeKey, redisPairingAttemptsKey},
		code, strconv.Itoa(s.maxAttempts),
	).Int()
	if err != nil {
		return false
	}
	return result == 1
}

// Ensure RedisPairingCodeStore satisfies the interface.
var _ PairingCodeStore = (*RedisPairingCodeStore)(nil)
var _ PairingCodeStore = (*memoryPairingCodeStore)(nil)

// Ensure RedisTokenCache satisfies the interface.
var _ TokenCache = (*RedisTokenCache)(nil)
var _ TokenCache = (*memoryTokenCache)(nil)

// devicePairingStoreRedis is declared in device_pairing_store_redis.go.
// oauthStateStoreRedis is declared in oauth_state_redis.go.
// Verify replay cache interface satisfaction.
var _ error = errors.New("") // keep errors import
