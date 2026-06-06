package llmproxy

import (
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/taskcheckout"
	"github.com/redis/go-redis/v9"
)

type TaskCheckoutKey = taskcheckout.Key
type TaskCheckout = taskcheckout.Checkout
type TaskCheckoutStore = taskcheckout.Store
type MemoryTaskCheckoutStore = taskcheckout.MemoryStore
type RedisTaskCheckoutStore = taskcheckout.RedisStore

func NewMemoryTaskCheckoutStore(defaultTTL time.Duration) *MemoryTaskCheckoutStore {
	return taskcheckout.NewMemoryStore(defaultTTL)
}

func NewRedisTaskCheckoutStore(rdb *redis.Client, defaultTTL time.Duration) *RedisTaskCheckoutStore {
	return taskcheckout.NewRedisStore(rdb, defaultTTL)
}
