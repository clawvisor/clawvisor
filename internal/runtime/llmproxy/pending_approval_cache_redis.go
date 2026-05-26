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
	// Key TTL must be at least as long as the longest per-hold
	// ExpiresAt currently sitting in the key — otherwise Redis evicts
	// the entire list (and every hold in it) before its holds expire.
	// c.ttl is the floor for holds that don't set their own ExpiresAt
	// (most tool-stage holds); per-hold ExpiresAt may extend beyond
	// that floor (inline-task approval holds use a 24h ExpiresAt to
	// give the user an overnight decide window).
	//
	// The push+trim+ttl is one Lua script so the conditional-EXPIRE
	// observes the same key state as the LPush — and so a sibling
	// 10-min hold pushed onto a key that already has a 24h hold can't
	// drop the key TTL below 24h.
	keyTTL := c.ttl
	if !pending.ExpiresAt.IsZero() {
		if d := pending.ExpiresAt.Sub(now); d > keyTTL {
			keyTTL = d
		}
	}
	if _, err := redisPendingApprovalHoldScript.Run(
		ctx, c.rdb, []string{key},
		raw, max, keyTTL.Milliseconds(),
	).Result(); err != nil && !errors.Is(err, redis.Nil) {
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
		// Bare reply: only the newest valid hold qualifies. If the
		// caller passed a Stage filter and the newest's stage
		// doesn't match, bail rather than walking back to find an
		// older same-stage hold — the user's "approve" / "deny" /
		// "task" pertains to the LAST prompt the harness rendered,
		// not to a stale earlier one of the right shape.
		if req.Stage != "" && pending.Stage != req.Stage {
			break
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

// redisPendingApprovalHoldScript performs LPush + LTrim + conditional
// PEXPIRE atomically. The PEXPIRE only fires when it would EXTEND the
// current TTL — never shorten it. Treats "no TTL" (-1) and "missing"
// (-2) as zero so a freshly LPush'd key always gets an initial TTL.
// (ExpireGT alone won't do this: it treats a no-TTL key as infinite
// and thus refuses to set the first TTL.)
var redisPendingApprovalHoldScript = redis.NewScript(`
local key = KEYS[1]
local raw = ARGV[1]
local max_len = tonumber(ARGV[2])
local target_ms = tonumber(ARGV[3])
redis.call('LPUSH', key, raw)
redis.call('LTRIM', key, 0, max_len - 1)
local current_ms = redis.call('PTTL', key)
if current_ms == -1 or current_ms == -2 or current_ms < target_ms then
  redis.call('PEXPIRE', key, target_ms)
end
return 1
`)

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
		else
			-- Bare reply: only the newest valid hold (the first
			-- one we reach after skipping invalid JSON) qualifies.
			-- If a Stage filter is set and this hold's stage
			-- doesn't match, return nil rather than walking past
			-- to find an older same-stage hold. Expired-by-
			-- ExpiresAt is handled by the Go caller's retry loop.
			if stage ~= '' and pending_stage ~= stage then
				return nil
			end
			redis.call('LSET', key, i, marker)
			redis.call('LREM', key, 1, marker)
			return raw
		end
	end
end
return nil
`)

var _ PendingApprovalCache = (*RedisPendingApprovalCache)(nil)
