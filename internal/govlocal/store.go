package govlocal

import (
	"context"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// LocalOrgID is the sentinel OrgID the pipeline carries for every agent in
// a pure-OSS build so instance-scoped callbacks actually fire. The
// pipeline policies no-op when the resolved orgID is "" (see
// org_model_policy.go); the run.go wiring installs a resolver returning
// this constant whenever local governance is active and no cloud
// OrgIDForAgent is provided. It is never persisted (the violation table
// has no org_id column) and must never leak into cloud builds.
const LocalOrgID = "local"

// spendCacheTTL is how long a computed window spend sum is cached
// in-process. Short enough that a cap change is felt quickly, long enough
// to spare the DB a SUM() on every proxied request. Matches the spec's
// 60s window.
const spendCacheTTL = 60 * time.Second

// spendCache memoizes the instance-wide cost sum per window (daily /
// monthly) for spendCacheTTL. Plain mutex + timestamp — no new
// dependency. Cached per window because the daily and monthly windows
// have different bounds and roll over independently.
type spendCache struct {
	mu      sync.Mutex
	entries map[string]spendCacheEntry
}

type spendCacheEntry struct {
	sum      int64
	computed time.Time
}

func newSpendCache() *spendCache {
	return &spendCache{entries: make(map[string]spendCacheEntry)}
}

// sum returns the cost sum for [since, until), using the cached value when
// it is younger than spendCacheTTL, otherwise recomputing via the store.
// The window key distinguishes the daily/monthly caches.
func (c *spendCache) sum(ctx context.Context, st store.Store, window string, since, until time.Time) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[window]; ok && time.Since(e.computed) < spendCacheTTL {
		return e.sum, nil
	}
	total, err := st.SumInstanceCostMicros(ctx, since, until)
	if err != nil {
		return 0, err
	}
	c.entries[window] = spendCacheEntry{sum: total, computed: time.Now()}
	return total, nil
}

// windowBounds returns the start/end of the current daily or monthly
// window. "daily" is the calendar day in UTC; "monthly" is the calendar
// month in UTC. Ported from cloud callbacks.go windowBounds — the OSS and
// cloud spend windows MUST agree so a cap behaves identically across
// tiers.
func windowBounds(window string) (since, until time.Time) {
	now := time.Now().UTC()
	switch window {
	case "monthly":
		since = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		until = since.AddDate(0, 1, 0)
	default: // daily
		since = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		until = since.Add(24 * time.Hour)
	}
	return since, until
}
