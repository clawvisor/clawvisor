package slack

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// detailTTL is how long a resolved prompt's "View request details" button
// keeps working. Longer than the approval itself: the button is a record of
// what happened, and someone reading back through a channel weeks later is
// exactly who needs it.
const detailTTL = 30 * 24 * time.Hour

const redisDetailPrefix = "clawvisor:slackdt:"

// detailOpTimeout is deliberately shorter than redisOpTimeout. A detail read
// happens on the trigger_id path, and Slack invalidates a trigger_id after
// three seconds — a read that outlived it would succeed and then have the
// modal rejected anyway, so failing fast leaves room for the config read
// that follows.
const detailOpTimeout = 1500 * time.Millisecond

// DetailStorer holds the request detail behind a resolved prompt's
// "View request details" button.
//
// Slack has no collapsible block, so the only way to keep the detail out of
// the channel while keeping it reachable is to render it into a modal on
// demand. That means the blocks have to outlive the resolve, and be readable
// by whichever replica handles the click — which is generally not the one
// that posted the prompt.
type DetailStorer interface {
	PutDetail(ctx context.Context, token string, d DetailEntry, ttl time.Duration) error
	GetDetail(ctx context.Context, token string) (DetailEntry, bool)
	Cleanup()
}

// DetailEntry is what sits behind the button. UserID is carried because the
// click arrives with no Clawvisor identity of its own — only a Slack user and
// a token — and opening the modal needs that account's bot token. TeamID is
// checked against the clicking workspace so a token cannot be replayed into a
// different one.
type DetailEntry struct {
	UserID string  `json:"user_id"`
	TeamID string  `json:"team_id"`
	Blocks []block `json:"blocks"`
}

type memoryDetailEntry struct {
	entry     DetailEntry
	expiresAt time.Time
}

type memoryDetailStore struct {
	mu sync.Mutex
	m  map[string]memoryDetailEntry
}

func newMemoryDetailStore() *memoryDetailStore {
	return &memoryDetailStore{m: make(map[string]memoryDetailEntry)}
}

func (s *memoryDetailStore) PutDetail(_ context.Context, token string, d DetailEntry, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[token] = memoryDetailEntry{entry: d, expiresAt: time.Now().Add(ttl)}
	return nil
}

func (s *memoryDetailStore) GetDetail(_ context.Context, token string) (DetailEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[token]
	if !ok || time.Now().After(e.expiresAt) {
		return DetailEntry{}, false
	}
	return e.entry, true
}

func (s *memoryDetailStore) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, e := range s.m {
		if now.After(e.expiresAt) {
			delete(s.m, k)
		}
	}
}

// redisDetailStore shares detail across replicas, so the button keeps working
// no matter which instance handles the click — and keeps working after the
// instance that posted the prompt is gone, which with min_instances=0 is
// most of the time.
type redisDetailStore struct {
	rdb *redis.Client
}

// NewRedisDetailStore creates a Redis-backed detail store.
func NewRedisDetailStore(rdb *redis.Client) DetailStorer {
	return &redisDetailStore{rdb: rdb}
}

func (s *redisDetailStore) PutDetail(ctx context.Context, token string, d DetailEntry, ttl time.Duration) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	return s.rdb.Set(ctx, redisDetailPrefix+token, data, ttl).Err()
}

func (s *redisDetailStore) GetDetail(ctx context.Context, token string) (DetailEntry, bool) {
	ctx, cancel := context.WithTimeout(ctx, detailOpTimeout)
	defer cancel()
	data, err := s.rdb.Get(ctx, redisDetailPrefix+token).Bytes()
	if err != nil {
		// A backend error and an expired key are both "no longer
		// available" to the caller, so they are not distinguished here.
		return DetailEntry{}, false
	}
	var d DetailEntry
	if err := json.Unmarshal(data, &d); err != nil {
		return DetailEntry{}, false
	}
	return d, true
}

// Cleanup is a no-op — Redis key TTLs handle expiry.
func (s *redisDetailStore) Cleanup() {}

var (
	_ DetailStorer = (*memoryDetailStore)(nil)
	_ DetailStorer = (*redisDetailStore)(nil)
)
