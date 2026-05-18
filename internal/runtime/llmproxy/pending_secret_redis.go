package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/redis/go-redis/v9"
)

const (
	redisPendingSecretOrderPrefix = "clawvisor:lite_secret_decision:scope:"
	redisPendingSecretDataPrefix  = "clawvisor:lite_secret_decision:item:"
)

// RedisPendingSecretDecisionCache stores proxy-lite pending secret decisions in
// Redis so a decision held on one API instance can be consumed on another.
type RedisPendingSecretDecisionCache struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewRedisPendingSecretDecisionCache(rdb *redis.Client, ttl time.Duration) *RedisPendingSecretDecisionCache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &RedisPendingSecretDecisionCache{rdb: rdb, ttl: ttl}
}

func (c *RedisPendingSecretDecisionCache) HoldSecret(ctx context.Context, pending PendingSecretDecision) (PendingSecretDecision, error) {
	if c == nil || c.rdb == nil {
		return pending, nil
	}
	now := time.Now().UTC()
	if pending.ID == "" {
		id, err := newSecretDecisionID()
		if err != nil {
			return PendingSecretDecision{}, err
		}
		pending.ID = id
	}
	if pending.CreatedAt.IsZero() {
		pending.CreatedAt = now
	}
	if pending.ExpiresAt.IsZero() {
		pending.ExpiresAt = now.Add(c.ttl)
	}
	raw, err := json.Marshal(pending)
	if err != nil {
		return PendingSecretDecision{}, err
	}
	ttl := time.Until(pending.ExpiresAt)
	if ttl <= 0 {
		ttl = c.ttl
	}
	orderKey := redisPendingSecretOrderKey(pending.UserID, pending.AgentID, pending.Provider)
	dataKey := redisPendingSecretDataKey(pending.UserID, pending.AgentID, pending.Provider, pending.ID)
	pipe := c.rdb.TxPipeline()
	pipe.Set(ctx, dataKey, raw, ttl)
	pipe.ZAdd(ctx, orderKey, redis.Z{Score: float64(pending.CreatedAt.UnixNano()), Member: pending.ID})
	pipe.Expire(ctx, orderKey, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return PendingSecretDecision{}, err
	}
	return pending, nil
}

func (c *RedisPendingSecretDecisionCache) PeekSecret(ctx context.Context, userID, agentID string, provider conversation.Provider) (*PendingSecretDecision, error) {
	if c == nil || c.rdb == nil {
		return nil, nil
	}
	for {
		ids, err := c.rdb.ZRevRange(ctx, redisPendingSecretOrderKey(userID, agentID, provider), 0, 0).Result()
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, nil
		}
		pending, err := c.get(ctx, userID, agentID, provider, ids[0])
		if err == nil {
			return pending, nil
		}
		if !errors.Is(err, redis.Nil) {
			return nil, err
		}
		_ = c.rdb.ZRem(ctx, redisPendingSecretOrderKey(userID, agentID, provider), ids[0]).Err()
	}
}

func (c *RedisPendingSecretDecisionCache) ResolveSecret(ctx context.Context, userID, agentID string, provider conversation.Provider) (*PendingSecretDecision, error) {
	if c == nil || c.rdb == nil {
		return nil, nil
	}
	result, err := redisResolveLatestPendingSecretScript.Run(ctx, c.rdb, []string{
		redisPendingSecretOrderKey(userID, agentID, provider),
		redisPendingSecretDataPrefix + redisPendingSecretScope(userID, agentID, provider) + ":",
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	raw, ok := result.(string)
	if !ok {
		return nil, fmt.Errorf("redis pending secret script returned %T", result)
	}
	return decodeRedisPendingSecret([]byte(raw))
}

func (c *RedisPendingSecretDecisionCache) ResolveSecretID(ctx context.Context, userID, agentID string, provider conversation.Provider, id string) (*PendingSecretDecision, error) {
	if c == nil || c.rdb == nil || id == "" {
		return nil, nil
	}
	dataKey := redisPendingSecretDataKey(userID, agentID, provider, id)
	raw, err := c.rdb.GetDel(ctx, dataKey).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	if err := c.rdb.ZRem(ctx, redisPendingSecretOrderKey(userID, agentID, provider), id).Err(); err != nil {
		return nil, err
	}
	return decodeRedisPendingSecret(raw)
}

func (c *RedisPendingSecretDecisionCache) get(ctx context.Context, userID, agentID string, provider conversation.Provider, id string) (*PendingSecretDecision, error) {
	raw, err := c.rdb.Get(ctx, redisPendingSecretDataKey(userID, agentID, provider, id)).Bytes()
	if err != nil {
		return nil, err
	}
	return decodeRedisPendingSecret(raw)
}

func decodeRedisPendingSecret(raw []byte) (*PendingSecretDecision, error) {
	var pending PendingSecretDecision
	if err := json.Unmarshal(raw, &pending); err != nil {
		return nil, err
	}
	return &pending, nil
}

var redisResolveLatestPendingSecretScript = redis.NewScript(`
local ids = redis.call('ZREVRANGE', KEYS[1], 0, -1)
for _, id in ipairs(ids) do
	local data_key = KEYS[2] .. id
	local raw = redis.call('GET', data_key)
	redis.call('ZREM', KEYS[1], id)
	if raw then
		redis.call('DEL', data_key)
		return raw
	end
end
return nil
`)

func redisPendingSecretOrderKey(userID, agentID string, provider conversation.Provider) string {
	return redisPendingSecretOrderPrefix + redisPendingSecretScope(userID, agentID, provider)
}

func redisPendingSecretDataKey(userID, agentID string, provider conversation.Provider, id string) string {
	return redisPendingSecretDataPrefix + redisPendingSecretScope(userID, agentID, provider) + ":" + id
}

func redisPendingSecretScope(userID, agentID string, provider conversation.Provider) string {
	return fmt.Sprintf("%s:%s:%s", userID, agentID, provider)
}

var _ PendingSecretDecisionCache = (*RedisPendingSecretDecisionCache)(nil)
