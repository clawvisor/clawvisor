package slack

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisReplayPrefix = "clawvisor:slackrp:"

// memoryReplayGuard rejects a repeated request signature within one process.
//
// This replaces the previous no-op default. The Slack signature is valid for
// the whole 5-minute skew window, so without a guard a captured payload can
// be resubmitted freely inside it; on a single-instance deployment an
// in-process set closes that entirely.
type memoryReplayGuard struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newMemoryReplayGuard() *memoryReplayGuard {
	return &memoryReplayGuard{seen: make(map[string]time.Time)}
}

func (g *memoryReplayGuard) SeenBefore(_ context.Context, sig string, ttl time.Duration) (bool, error) {
	now := time.Now()

	g.mu.Lock()
	defer g.mu.Unlock()

	// Sweep opportunistically; entries are only useful for the skew window
	// and the map would otherwise grow for the process lifetime.
	for k, exp := range g.seen {
		if now.After(exp) {
			delete(g.seen, k)
		}
	}

	if exp, ok := g.seen[sig]; ok && now.Before(exp) {
		return true, nil
	}
	g.seen[sig] = now.Add(ttl)
	return false, nil
}

// redisReplayGuard shares seen signatures across replicas. Without it a
// captured payload can simply be retried until it lands on a replica that
// has not seen it.
type redisReplayGuard struct {
	rdb *redis.Client
}

// NewRedisReplayGuard creates a Redis-backed replay guard.
func NewRedisReplayGuard(rdb *redis.Client) ReplayGuard {
	return &redisReplayGuard{rdb: rdb}
}

func (g *redisReplayGuard) SeenBefore(ctx context.Context, sig string, ttl time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()

	// SetNX is atomic across replicas: exactly one caller sees "not seen".
	ok, err := g.rdb.SetNX(ctx, redisReplayPrefix+sig, "1", ttl).Result()
	if err != nil {
		// Surface the error rather than guessing. Reporting "not seen"
		// would void replay protection precisely when the shared guard
		// matters most, and the handler fails the interaction closed on a
		// non-nil error.
		return false, err
	}
	return !ok, nil
}

var (
	_ ReplayGuard = (*memoryReplayGuard)(nil)
	_ ReplayGuard = (*redisReplayGuard)(nil)
)
