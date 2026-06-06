package callernonce

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisCallerNoncePrefix = "clawvisor:caller_nonce:"

// RedisCallerNonceCache stores caller nonces in Redis with per-key TTL,
// enabling cross-instance consumption so a nonce minted on one daemon
// can be consumed on another. GETDEL (Redis ≥ 6.2) gives atomic
// one-shot semantics without a Lua script.
type RedisCallerNonceCache struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewRedisCallerNonceCache returns a Redis-backed nonce cache. ttl <= 0
// is replaced with 5 minutes.
func NewRedisCallerNonceCache(rdb *redis.Client, ttl time.Duration) *RedisCallerNonceCache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &RedisCallerNonceCache{rdb: rdb, ttl: ttl}
}

type redisNoncePayload struct {
	AgentID string `json:"agent_id"`
	Host    string `json:"host"`
	Method  string `json:"method"`
	Path    string `json:"path"`
}

// Mint implements CallerNonceCache.
func (c *RedisCallerNonceCache) Mint(ctx context.Context, agentID string, target NonceTarget) (string, error) {
	nonce, err := generateNonce()
	if err != nil {
		return "", err
	}
	target = normalizeNonceTarget(target)
	// Mirror the memory impl's normalization so consumers observe the
	// same agent ID shape regardless of which backend is configured.
	agentID = strings.TrimSpace(agentID)
	payload, err := json.Marshal(redisNoncePayload{
		AgentID: agentID,
		Host:    target.Host,
		Method:  target.Method,
		Path:    target.Path,
	})
	if err != nil {
		return "", err
	}
	if err := c.rdb.Set(ctx, redisCallerNoncePrefix+nonce, payload, c.ttl).Err(); err != nil {
		return "", err
	}
	return nonce, nil
}

// Consume implements CallerNonceCache.
//
// GETDEL is atomic. After the call returns, the key is gone whether or
// not the target validates — same one-shot semantic as the memory impl.
// A mismatched target therefore can't be retried with the same nonce.
func (c *RedisCallerNonceCache) Consume(ctx context.Context, nonce string, target NonceTarget) (string, error) {
	target = normalizeNonceTarget(target)
	res := c.rdb.GetDel(ctx, redisCallerNoncePrefix+nonce)
	raw, err := res.Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", ErrNonceNotFound
		}
		return "", err
	}
	var payload redisNoncePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		// Corrupt entry — treat as missing rather than authorizing on
		// half-parsed state.
		return "", ErrNonceNotFound
	}
	stored := NonceTarget{
		Host:   payload.Host,
		Method: payload.Method,
		Path:   payload.Path,
	}
	if stored != target {
		return "", ErrNonceTargetMismatch
	}
	return payload.AgentID, nil
}

var _ CallerNonceCache = (*RedisCallerNonceCache)(nil)
