package review

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisHeldApprovalOrderPrefix = "clawvisor:runtime:held_approval:session:"
	redisHeldApprovalDataPrefix  = "clawvisor:runtime:held_approval:item:"
)

type RedisApprovalCache struct {
	rdb     *redis.Client
	IdleTTL time.Duration
	nowFn   func() time.Time
}

func NewRedisApprovalCache(rdb *redis.Client) HeldApprovalCache {
	if rdb == nil {
		return NewApprovalCache()
	}
	return &RedisApprovalCache{
		rdb:     rdb,
		IdleTTL: 30 * time.Minute,
		nowFn:   time.Now,
	}
}

func (c *RedisApprovalCache) Hold(sessionID, approvalRecordID, taskID, toolUseID, toolName string, toolInput map[string]any, reason string) (*HeldApproval, bool) {
	if c == nil || c.rdb == nil || sessionID == "" || approvalRecordID == "" {
		return nil, false
	}
	if existing := c.GetByApprovalRecord(sessionID, approvalRecordID); existing != nil {
		return existing, false
	}
	now := c.nowFn().UTC()
	held := &HeldApproval{
		ID:               approvalRecordID,
		ApprovalRecordID: approvalRecordID,
		TaskID:           taskID,
		ToolUseID:        toolUseID,
		ToolName:         toolName,
		ToolInput:        toolInput,
		Reason:           reason,
		CreatedAt:        now,
	}
	raw, err := json.Marshal(held)
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ok, err := c.rdb.SetNX(ctx, c.dataKey(sessionID, approvalRecordID), raw, c.ttl()).Result()
	if err != nil {
		return nil, false
	}
	if !ok {
		if existing := c.GetByApprovalRecord(sessionID, approvalRecordID); existing != nil {
			return existing, false
		}
		return nil, false
	}
	pipe := c.rdb.Pipeline()
	pipe.ZAdd(ctx, c.sessionKey(sessionID), redis.Z{Score: float64(now.UnixNano()), Member: approvalRecordID})
	pipe.Expire(ctx, c.sessionKey(sessionID), c.ttl())
	_, _ = pipe.Exec(ctx)
	return held, true
}

func (c *RedisApprovalCache) Get(sessionID string) *HeldApproval {
	held := c.List(sessionID)
	if len(held) == 0 {
		return nil
	}
	return held[0]
}

func (c *RedisApprovalCache) GetByApprovalRecord(sessionID, approvalRecordID string) *HeldApproval {
	if c == nil || c.rdb == nil || sessionID == "" || approvalRecordID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := c.rdb.Get(ctx, c.dataKey(sessionID, approvalRecordID)).Bytes()
	if err != nil {
		return nil
	}
	var held HeldApproval
	if err := json.Unmarshal(raw, &held); err != nil {
		return nil
	}
	return &held
}

func (c *RedisApprovalCache) Resolve(sessionID, id string) *HeldApproval {
	held := c.getByID(sessionID, id)
	if held == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pipe := c.rdb.Pipeline()
	pipe.Del(ctx, c.dataKey(sessionID, held.ApprovalRecordID))
	pipe.ZRem(ctx, c.sessionKey(sessionID), held.ApprovalRecordID)
	_, _ = pipe.Exec(ctx)
	return held
}

func (c *RedisApprovalCache) Drop(sessionID string, ids ...string) {
	if c == nil || c.rdb == nil || sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if len(ids) == 0 {
		ids, _ = c.memberIDs(ctx, sessionID)
	}
	if len(ids) == 0 {
		return
	}
	pipe := c.rdb.Pipeline()
	for _, id := range ids {
		held := c.getByID(sessionID, id)
		if held == nil {
			continue
		}
		pipe.Del(ctx, c.dataKey(sessionID, held.ApprovalRecordID))
		pipe.ZRem(ctx, c.sessionKey(sessionID), held.ApprovalRecordID)
	}
	_, _ = pipe.Exec(ctx)
}

func (c *RedisApprovalCache) RebindTask(sessionID, approvalRecordID, taskID string) bool {
	if c == nil || c.rdb == nil {
		return false
	}
	held := c.GetByApprovalRecord(sessionID, approvalRecordID)
	if held == nil {
		return false
	}
	held.TaskID = taskID
	raw, err := json.Marshal(held)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ttl, err := c.rdb.TTL(ctx, c.dataKey(sessionID, approvalRecordID)).Result()
	if err != nil || ttl <= 0 {
		ttl = c.ttl()
	}
	return c.rdb.Set(ctx, c.dataKey(sessionID, approvalRecordID), raw, ttl).Err() == nil
}

func (c *RedisApprovalCache) Count(sessionID string) int {
	return len(c.List(sessionID))
}

func (c *RedisApprovalCache) List(sessionID string) []*HeldApproval {
	if c == nil || c.rdb == nil || sessionID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ids, err := c.memberIDs(ctx, sessionID)
	if err != nil || len(ids) == 0 {
		return nil
	}
	out := make([]*HeldApproval, 0, len(ids))
	var stale []string
	for _, id := range ids {
		held := c.getByID(sessionID, id)
		if held == nil {
			stale = append(stale, id)
			continue
		}
		out = append(out, held)
	}
	if len(stale) > 0 {
		_ = c.rdb.ZRem(ctx, c.sessionKey(sessionID), stringSliceToInterface(stale)...).Err()
	}
	return out
}

func (c *RedisApprovalCache) memberIDs(ctx context.Context, sessionID string) ([]string, error) {
	return c.rdb.ZRange(ctx, c.sessionKey(sessionID), 0, -1).Result()
}

func (c *RedisApprovalCache) getByID(sessionID, id string) *HeldApproval {
	if id == "" {
		return nil
	}
	return c.GetByApprovalRecord(sessionID, id)
}

func (c *RedisApprovalCache) ttl() time.Duration {
	if c == nil || c.IdleTTL <= 0 {
		return 30 * time.Minute
	}
	return c.IdleTTL
}

func (c *RedisApprovalCache) sessionKey(sessionID string) string {
	return redisHeldApprovalOrderPrefix + sessionID
}

func (c *RedisApprovalCache) dataKey(sessionID, approvalRecordID string) string {
	return fmt.Sprintf("%s%s:%s", redisHeldApprovalDataPrefix, sessionID, approvalRecordID)
}

func stringSliceToInterface(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

var _ HeldApprovalCache = (*RedisApprovalCache)(nil)
