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
	redisGuardPrefix     = "clawvisor:slackcbg:"
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
	// GuardID names the one key the approve/deny pair shares. It is what
	// makes first-responder-wins enforceable across replicas; see Consume.
	GuardID string `json:"guard_id,omitempty"`
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
	guardID, err := randomShortID()
	if err != nil {
		return "", "", err
	}

	expiresAt := time.Now().Add(ttl).UnixMilli()
	mk := func(sibling string) ([]byte, error) {
		return json.Marshal(redisCallbackEntry{
			Type: entryType, TargetID: targetID, TaskID: taskID,
			UserID: userID, ChannelID: channelID,
			ExpiresAt: expiresAt, SiblingID: sibling, GuardID: guardID,
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
	// Transactional, not a plain pipeline: a pair written without its guard
	// would be unusable rather than merely unshared, because Consume reads a
	// missing guard as "someone already won". The three keys have to land
	// together or not at all.
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, redisTokenPrefix+approveID, approveData, keyTTL)
	pipe.Set(ctx, redisTokenPrefix+denyID, denyData, keyTTL)
	pipe.Set(ctx, redisGuardPrefix+guardID, "1", keyTTL)
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

// Consume retires a whole approve/deny pair and returns its entry, or reports
// why the token can no longer be used.
//
// The invariant is first-responder-wins: a request must never come back both
// approved and denied. Approve and deny live under separate keys, so a
// per-key GetDel cannot establish it — two replicas handling the two halves
// of one pair concurrently would each win their own key and each retire the
// other's, and the decision consumer would see both outcomes. Arbitration
// therefore happens on the single key the pair shares, the guard: exactly one
// caller can take its value, and everyone else is a late click no matter
// which button they pressed.
func (s *redisCallbackTokenStore) Consume(shortID string) (*callbackEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()

	// Read without retiring. The token has to outlive a losing attempt, or a
	// loser would delete state the eventual winner still needs to resolve.
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

	if j.GuardID == "" {
		return s.consumeUnguarded(ctx, shortID, j)
	}

	// The guard and the token keys share a TTL, so having read a live entry
	// above, an absent guard can only mean it was taken — never that it
	// lapsed on its own.
	if _, err := s.rdb.GetDel(ctx, redisGuardPrefix+j.GuardID).Result(); err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, errTokenUsed
		}
		return nil, err
	}

	s.retirePair(ctx, shortID, j.SiblingID)
	return j.entry(), nil
}

// consumeUnguarded resolves a token minted before pairs carried a guard.
//
// Callback tokens live for a day, so a deploy of the guard leaves prompts in
// channels whose tokens predate it. Reading their absent guard as "already
// won" would wedge every one of them; falling back to the old per-key GetDel
// keeps them clickable, and is no weaker than the behaviour they were posted
// under. Removable once one callback TTL has passed since rollout.
func (s *redisCallbackTokenStore) consumeUnguarded(ctx context.Context, shortID string, j redisCallbackEntry) (*callbackEntry, error) {
	if _, err := s.rdb.GetDel(ctx, redisTokenPrefix+shortID).Result(); err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, s.tombstoneReason(ctx, shortID)
		}
		return nil, err
	}
	s.retirePair(ctx, shortID, j.SiblingID)
	return j.entry(), nil
}

// retirePair deletes both halves of a pair and tombstones them, so a later
// click on either button is told the request was resolved rather than that
// its token never existed.
func (s *redisCallbackTokenStore) retirePair(ctx context.Context, shortID, siblingID string) {
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, redisTokenPrefix+shortID)
	pipe.Set(ctx, redisTombstonePrefix+shortID, tombstoneUsed, tombstoneGrace)
	if siblingID != "" {
		pipe.Del(ctx, redisTokenPrefix+siblingID)
		pipe.Set(ctx, redisTombstonePrefix+siblingID, tombstoneUsed, tombstoneGrace)
	}
	_, _ = pipe.Exec(ctx)
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
