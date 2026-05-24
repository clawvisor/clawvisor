package llmproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/redis/go-redis/v9"
)

const redisPendingApprovalPrefix = "clawvisor:lite_pending_approval:"

// RedisPendingApprovalCache stores lite-proxy inline approval holds in Redis
// so a dedicated or multi-replica proxy deployment can release a hold created
// by another instance. The list is newest-first and bounded to match the
// in-memory cache's LIFO behavior.
type RedisPendingApprovalCache struct {
	rdb *redis.Client
	ttl time.Duration
	max int
	now func() time.Time
}

// NewRedisPendingApprovalCache returns a Redis-backed pending approval cache.
// ttl <= 0 is replaced with 10 minutes.
func NewRedisPendingApprovalCache(rdb *redis.Client, ttl time.Duration) *RedisPendingApprovalCache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &RedisPendingApprovalCache{
		rdb: rdb,
		ttl: ttl,
		max: 10,
		now: time.Now,
	}
}

// Hold implements PendingApprovalCache.
func (c *RedisPendingApprovalCache) Hold(ctx context.Context, pending PendingLiteApproval) (HoldResult, error) {
	if c == nil || c.rdb == nil {
		return HoldResult{Pending: pending}, nil
	}
	now := c.now().UTC()
	if pending.ID == "" {
		id, err := newLiteApprovalID()
		if err != nil {
			return HoldResult{}, err
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
		return HoldResult{}, err
	}
	key := redisPendingApprovalKey(pending.UserID, pending.AgentID, pending.Provider, pending.ConversationID)
	max := c.max
	if max <= 0 {
		max = 10
	}
	var evicted *PendingLiteApproval
	if rawEvicted, err := c.rdb.LIndex(ctx, key, int64(max-1)).Bytes(); err == nil {
		var decoded PendingLiteApproval
		if json.Unmarshal(rawEvicted, &decoded) == nil {
			evicted = &decoded
		}
	}
	pipe := c.rdb.TxPipeline()
	pipe.LPush(ctx, key, raw)
	pipe.LTrim(ctx, key, 0, int64(max-1))
	pipe.Expire(ctx, key, c.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return HoldResult{}, err
	}
	return HoldResult{Pending: pending, Evicted: evicted}, nil
}

// Peek implements PendingApprovalCache.
func (c *RedisPendingApprovalCache) Peek(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, error) {
	if c == nil || c.rdb == nil {
		return nil, nil
	}
	found, _, err := c.find(ctx, req)
	return found, err
}

// Resolve implements PendingApprovalCache.
func (c *RedisPendingApprovalCache) Resolve(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, error) {
	if c == nil || c.rdb == nil {
		return nil, nil
	}
	key := redisPendingApprovalKey(req.UserID, req.AgentID, req.Provider, req.ConversationID)
	for {
		result, err := redisResolvePendingApprovalScript.Run(ctx, c.rdb, []string{key},
			req.ApprovalID, string(req.Stage), redisPendingApprovalRemovalMarker(c.now()),
		).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				return nil, nil
			}
			return nil, err
		}
		raw, ok := result.(string)
		if !ok {
			return nil, fmt.Errorf("redis pending approval script returned %T", result)
		}
		var found PendingLiteApproval
		if err := json.Unmarshal([]byte(raw), &found); err != nil {
			continue
		}
		if !found.ExpiresAt.IsZero() && !found.ExpiresAt.After(c.now().UTC()) {
			continue
		}
		return &found, nil
	}
}

// Drop implements PendingApprovalCache.
func (c *RedisPendingApprovalCache) Drop(ctx context.Context, req ResolveRequest) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	key := redisPendingApprovalKey(req.UserID, req.AgentID, req.Provider, req.ConversationID)
	if req.ApprovalID == "" {
		return c.rdb.Del(ctx, key).Err()
	}
	_, raw, err := c.find(ctx, req)
	if err != nil || raw == "" {
		return err
	}
	return c.rdb.LRem(ctx, key, 1, raw).Err()
}

func (c *RedisPendingApprovalCache) find(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, string, error) {
	key := redisPendingApprovalKey(req.UserID, req.AgentID, req.Provider, req.ConversationID)
	rawItems, err := c.rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, "", err
	}
	now := c.now().UTC()
	var firstExpired []string
	var fallback *PendingLiteApproval
	var fallbackRaw string
	for _, raw := range rawItems {
		var pending PendingLiteApproval
		if err := json.Unmarshal([]byte(raw), &pending); err != nil {
			firstExpired = append(firstExpired, raw)
			continue
		}
		if !pending.ExpiresAt.IsZero() && !pending.ExpiresAt.After(now) {
			firstExpired = append(firstExpired, raw)
			continue
		}
		if req.ApprovalID != "" {
			if pending.ID != req.ApprovalID {
				continue
			}
			if req.Stage != "" && pending.Stage != req.Stage {
				break
			}
			return &pending, raw, c.dropExpired(ctx, key, firstExpired)
		}
		if req.Stage != "" && pending.Stage != req.Stage {
			continue
		}
		fallback = &pending
		fallbackRaw = raw
		break
	}
	return fallback, fallbackRaw, c.dropExpired(ctx, key, firstExpired)
}

func (c *RedisPendingApprovalCache) dropExpired(ctx context.Context, key string, raws []string) error {
	if len(raws) == 0 {
		return nil
	}
	pipe := c.rdb.TxPipeline()
	for _, raw := range raws {
		pipe.LRem(ctx, key, 0, raw)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func redisPendingApprovalKey(userID, agentID string, provider conversation.Provider, conversationID string) string {
	// ConversationID partitions holds per-conversation. When empty, we
	// keep the legacy (user, agent, provider) shape unchanged so existing
	// redis entries from pre-conversation-scoping clients remain readable
	// — no migration required, no silent loss of pending approvals on
	// upgrade.
	if conversationID == "" {
		sum := sha256.Sum256([]byte(userID + "\x00" + agentID + "\x00" + string(provider)))
		return redisPendingApprovalPrefix + hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256([]byte(userID + "\x00" + agentID + "\x00" + string(provider) + "\x00" + conversationID))
	return redisPendingApprovalPrefix + hex.EncodeToString(sum[:])
}

func redisPendingApprovalRemovalMarker(now time.Time) string {
	return "__clawvisor_removed_pending_approval__:" + now.UTC().Format(time.RFC3339Nano)
}

var redisResolvePendingApprovalScript = redis.NewScript(`
local key = KEYS[1]
local approval_id = ARGV[1]
local stage = ARGV[2]
local marker = ARGV[3]
local len = redis.call('LLEN', key)
for i = 0, len - 1 do
	local raw = redis.call('LINDEX', key, i)
	if not raw then
		return nil
	end
	local ok, pending = pcall(cjson.decode, raw)
	if not ok then
		redis.call('LSET', key, i, marker)
		redis.call('LREM', key, 1, marker)
	else
		local id = pending['ID'] or ''
		local pending_stage = pending['Stage'] or ''
		if approval_id ~= '' then
			if id == approval_id then
				if stage ~= '' and pending_stage ~= stage then
					return nil
				end
				redis.call('LSET', key, i, marker)
				redis.call('LREM', key, 1, marker)
				return raw
			end
		elseif stage == '' or pending_stage == stage then
			redis.call('LSET', key, i, marker)
			redis.call('LREM', key, 1, marker)
			return raw
		end
	end
end
return nil
`)

var _ PendingApprovalCache = (*RedisPendingApprovalCache)(nil)
