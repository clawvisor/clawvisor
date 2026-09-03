package slack

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisTokenPrefix     = "clawvisor:slackcb:"
	redisTombstonePrefix = "clawvisor:slackcbt:"
	redisOpTimeout       = 5 * time.Second
)

// Tombstone values, recording why a token stopped being usable so a late
// click can be told the truth rather than a generic "not available".
const (
	tombstoneUsed    = "used"
	tombstoneExpired = "expired"
)

// redisCallbackEntry is the JSON form stored in Redis.
type redisCallbackEntry struct {
	Type      string `json:"type"`
	TargetID  string `json:"target_id"`
	TaskID    string `json:"task_id,omitempty"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	ExpiresAt int64  `json:"expires_at"`
	SiblingID string `json:"sibling_id"`
}

// redisCallbackTokenStore shares Slack callback tokens across replicas.
//
// Slack posts an interaction to whichever replica the load balancer picks,
// which is generally not the one that posted the prompt. With the in-memory
// store that click finds no token and the user is told the request is no
// longer available, so this is required — not an optimisation — on any
// multi-instance deployment.
type redisCallbackTokenStore struct {
	rdb *redis.Client
}

// NewRedisCallbackTokenStore creates a Redis-backed callback token store.
func NewRedisCallbackTokenStore(rdb *redis.Client) CallbackTokenStorer {
	return &redisCallbackTokenStore{rdb: rdb}
}

func (s *redisCallbackTokenStore) Generate(entryType, targetID, userID, taskID, channelID string, ttl time.Duration) (string, string, error) {
	approveID, err := randomShortID()
	if err != nil {
		return "", "", err
	}
	denyID, err := randomShortID()
	if err != nil {
		return "", "", err
	}

	expiresAt := time.Now().Add(ttl).UnixMilli()
	mk := func(sibling string) ([]byte, error) {
		return json.Marshal(redisCallbackEntry{
			Type: entryType, TargetID: targetID, TaskID: taskID,
			UserID: userID, ChannelID: channelID,
			ExpiresAt: expiresAt, SiblingID: sibling,
		})
	}
	approveData, err := mk(denyID)
	if err != nil {
		return "", "", err
	}
	denyData, err := mk(approveID)
	if err != nil {
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()

	// Key TTL covers the tombstone window too, so an expired token is still
	// readable and can report itself as expired rather than unknown.
	keyTTL := ttl + tombstoneGrace
	pipe := s.rdb.Pipeline()
	pipe.Set(ctx, redisTokenPrefix+approveID, approveData, keyTTL)
	pipe.Set(ctx, redisTokenPrefix+denyID, denyData, keyTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return "", "", err
	}
	return approveID, denyID, nil
}

// Peek validates without retiring, so an unauthorized click cannot burn a
// live approval.
func (s *redisCallbackTokenStore) Peek(shortID string) (*callbackEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()

	data, err := s.rdb.Get(ctx, redisTokenPrefix+shortID).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, s.tombstoneReason(ctx, shortID)
	}
	if err != nil {
		return nil, err
	}

	var j redisCallbackEntry
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, errTokenNotFound
	}
	if time.Now().UnixMilli() > j.ExpiresAt {
		s.markTombstone(ctx, shortID, tombstoneExpired)
		return nil, errTokenExpired
	}
	return j.entry(), nil
}

func (s *redisCallbackTokenStore) Consume(shortID string) (*callbackEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()

	// GetDel is what makes first-responder-wins atomic across replicas:
	// only one caller can receive the value.
	data, err := s.rdb.GetDel(ctx, redisTokenPrefix+shortID).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, s.tombstoneReason(ctx, shortID)
	}
	if err != nil {
		return nil, err
	}

	var j redisCallbackEntry
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, errTokenNotFound
	}
	if time.Now().UnixMilli() > j.ExpiresAt {
		s.markTombstone(ctx, shortID, tombstoneExpired)
		return nil, errTokenExpired
	}

	// Retire the sibling so a request cannot be approved and also denied.
	if j.SiblingID != "" {
		s.rdb.Del(ctx, redisTokenPrefix+j.SiblingID)
		s.markTombstone(ctx, j.SiblingID, tombstoneUsed)
	}
	s.markTombstone(ctx, shortID, tombstoneUsed)

	return j.entry(), nil
}

// tombstoneReason maps an absent token onto why it went away.
func (s *redisCallbackTokenStore) tombstoneReason(ctx context.Context, shortID string) error {
	v, err := s.rdb.Get(ctx, redisTombstonePrefix+shortID).Result()
	if err != nil {
		return errTokenNotFound
	}
	switch v {
	case tombstoneUsed:
		return errTokenUsed
	case tombstoneExpired:
		return errTokenExpired
	default:
		return errTokenNotFound
	}
}

func (s *redisCallbackTokenStore) markTombstone(ctx context.Context, shortID, reason string) {
	s.rdb.Set(ctx, redisTombstonePrefix+shortID, reason, tombstoneGrace)
}

// Cleanup is a no-op — Redis key TTLs handle expiry.
func (s *redisCallbackTokenStore) Cleanup() {}

func (j redisCallbackEntry) entry() *callbackEntry {
	return &callbackEntry{
		Type:      j.Type,
		TargetID:  j.TargetID,
		TaskID:    j.TaskID,
		UserID:    j.UserID,
		ChannelID: j.ChannelID,
		ExpiresAt: time.UnixMilli(j.ExpiresAt),
	}
}

var _ CallbackTokenStorer = (*redisCallbackTokenStore)(nil)
