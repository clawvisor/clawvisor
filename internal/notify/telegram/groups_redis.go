package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

const (
	redisPendingGroupsPrefix  = "clawvisor:tgpg:"
	redisGroupPairingsPrefix  = "clawvisor:tggp:"
)

// PendingGroupStore abstracts storage of detected Telegram groups awaiting
// user approval.
type PendingGroupStore interface {
	Add(userID string, pg notify.PendingGroup)
	List(userID string) []notify.PendingGroup
	Remove(userID, chatID string)
}

// GroupPairingStore abstracts storage of agent-to-group pairing sessions.
type GroupPairingStore interface {
	Store(sessionID string, session *groupPairingSession)
	Load(sessionID string) (*groupPairingSession, bool)
	Delete(sessionID string)
}

// memoryPendingGroupStore wraps the existing sync.Map-based implementation.
type memoryPendingGroupStore struct {
	n *Notifier
}

func (s *memoryPendingGroupStore) Add(userID string, pg notify.PendingGroup) {
	s.n.addPendingGroupMemory(userID, pg)
}

func (s *memoryPendingGroupStore) List(userID string) []notify.PendingGroup {
	return s.n.pendingGroupsMemory(userID)
}

func (s *memoryPendingGroupStore) Remove(userID, chatID string) {
	s.n.removePendingGroupMemory(userID, chatID)
}

// memoryGroupPairingStore wraps the existing sync.Map-based implementation.
type memoryGroupPairingStore struct {
	n *Notifier
}

func (s *memoryGroupPairingStore) Store(sessionID string, session *groupPairingSession) {
	s.n.groupPairings.Store(sessionID, session)
	go func() {
		time.Sleep(5 * time.Minute)
		s.n.groupPairings.Delete(sessionID)
	}()
}

func (s *memoryGroupPairingStore) Load(sessionID string) (*groupPairingSession, bool) {
	val, ok := s.n.groupPairings.Load(sessionID)
	if !ok {
		return nil, false
	}
	return val.(*groupPairingSession), true
}

func (s *memoryGroupPairingStore) Delete(sessionID string) {
	s.n.groupPairings.Delete(sessionID)
}

// redisPendingGroupStore stores pending groups in Redis.
type redisPendingGroupStore struct {
	rdb *redis.Client
}

// NewRedisPendingGroupStore creates a Redis-backed pending group store.
func NewRedisPendingGroupStore(rdb *redis.Client) PendingGroupStore {
	return &redisPendingGroupStore{rdb: rdb}
}

func (s *redisPendingGroupStore) Add(userID string, pg notify.PendingGroup) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := redisPendingGroupsPrefix + userID

	// Check for duplicates by loading existing.
	existing := s.List(userID)
	for _, g := range existing {
		if g.ChatID == pg.ChatID {
			return
		}
	}

	data, err := json.Marshal(pg)
	if err != nil {
		return
	}
	_ = s.rdb.RPush(ctx, key, data).Err()
	// Keep pending groups for 24 hours.
	_ = s.rdb.Expire(ctx, key, 24*time.Hour).Err()
}

func (s *redisPendingGroupStore) List(userID string) []notify.PendingGroup {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := redisPendingGroupsPrefix + userID
	results, err := s.rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil || len(results) == 0 {
		return nil
	}

	groups := make([]notify.PendingGroup, 0, len(results))
	for _, raw := range results {
		var pg notify.PendingGroup
		if err := json.Unmarshal([]byte(raw), &pg); err != nil {
			continue
		}
		groups = append(groups, pg)
	}
	return groups
}

func (s *redisPendingGroupStore) Remove(userID, chatID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := redisPendingGroupsPrefix + userID
	results, err := s.rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return
	}
	for _, raw := range results {
		var pg notify.PendingGroup
		if err := json.Unmarshal([]byte(raw), &pg); err != nil {
			continue
		}
		if pg.ChatID == chatID {
			_ = s.rdb.LRem(ctx, key, 1, raw).Err()
		}
	}
}

// redisGroupPairingStore stores agent-group pairings in Redis with TTL.
type redisGroupPairingStore struct {
	rdb *redis.Client
}

// NewRedisGroupPairingStore creates a Redis-backed group pairing store.
func NewRedisGroupPairingStore(rdb *redis.Client) GroupPairingStore {
	return &redisGroupPairingStore{rdb: rdb}
}

func (s *redisGroupPairingStore) Store(sessionID string, session *groupPairingSession) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, _ := json.Marshal(session)
	_ = s.rdb.Set(ctx, redisGroupPairingsPrefix+sessionID, data, 5*time.Minute).Err()
}

func (s *redisGroupPairingStore) Load(sessionID string) (*groupPairingSession, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := s.rdb.Get(ctx, redisGroupPairingsPrefix+sessionID).Bytes()
	if err != nil {
		return nil, false
	}
	var session groupPairingSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, false
	}
	return &session, true
}

func (s *redisGroupPairingStore) Delete(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.rdb.Del(ctx, redisGroupPairingsPrefix+sessionID).Err()
}

// Compile-time checks.
var _ PendingGroupStore = (*redisPendingGroupStore)(nil)
var _ GroupPairingStore = (*redisGroupPairingStore)(nil)

// Ensure errors can be used.
var _ = fmt.Errorf
