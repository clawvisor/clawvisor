package llmproxy

import (
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/callernonce"
	"github.com/redis/go-redis/v9"
)

type CallerNonceCache = callernonce.CallerNonceCache
type NonceTarget = callernonce.NonceTarget
type MemoryCallerNonceCache = callernonce.MemoryCallerNonceCache
type RedisCallerNonceCache = callernonce.RedisCallerNonceCache

var (
	ErrNonceNotFound       = callernonce.ErrNonceNotFound
	ErrNonceTargetMismatch = callernonce.ErrNonceTargetMismatch
)

const NoncePrefix = callernonce.NoncePrefix

func NewMemoryCallerNonceCache(ttl time.Duration) *MemoryCallerNonceCache {
	return callernonce.NewMemoryCallerNonceCache(ttl)
}

func NewRedisCallerNonceCache(rdb *redis.Client, ttl time.Duration) *RedisCallerNonceCache {
	return callernonce.NewRedisCallerNonceCache(rdb, ttl)
}
