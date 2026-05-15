package llmproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisInlineApprovalOutcomePrefix = "clawvisor:lite_inline_approval_outcome:"

// RedisInlineApprovalOutcomeStore stores inline task approval outcomes in
// Redis so conversation-history augmentation works across proxy replicas.
type RedisInlineApprovalOutcomeStore struct {
	rdb *redis.Client
	ttl time.Duration
	now func() time.Time
}

func NewRedisInlineApprovalOutcomeStore(rdb *redis.Client, ttl time.Duration) *RedisInlineApprovalOutcomeStore {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &RedisInlineApprovalOutcomeStore{
		rdb: rdb,
		ttl: ttl,
		now: time.Now,
	}
}

func (s *RedisInlineApprovalOutcomeStore) Record(key InlineApprovalOutcomeKey, outcome InlineApprovalOutcome) {
	if s == nil || s.rdb == nil || key.ApprovalID == "" {
		return
	}
	if outcome.ResolvedAt.IsZero() {
		outcome.ResolvedAt = s.now().UTC()
	}
	raw, err := json.Marshal(outcome)
	if err != nil {
		return
	}
	_ = s.rdb.Set(context.Background(), redisInlineApprovalOutcomeKey(key), raw, s.ttl).Err()
}

func (s *RedisInlineApprovalOutcomeStore) Lookup(key InlineApprovalOutcomeKey) (InlineApprovalOutcome, bool) {
	if s == nil || s.rdb == nil || key.ApprovalID == "" {
		return InlineApprovalOutcome{}, false
	}
	raw, err := s.rdb.Get(context.Background(), redisInlineApprovalOutcomeKey(key)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return InlineApprovalOutcome{}, false
		}
		return InlineApprovalOutcome{}, false
	}
	var outcome InlineApprovalOutcome
	if err := json.Unmarshal(raw, &outcome); err != nil {
		return InlineApprovalOutcome{}, false
	}
	return outcome, true
}

func redisInlineApprovalOutcomeKey(key InlineApprovalOutcomeKey) string {
	sum := sha256.Sum256([]byte(key.UserID + "\x00" + key.AgentID + "\x00" + key.ApprovalID))
	return redisInlineApprovalOutcomePrefix + hex.EncodeToString(sum[:])
}

var _ InlineApprovalOutcomeStore = (*RedisInlineApprovalOutcomeStore)(nil)
