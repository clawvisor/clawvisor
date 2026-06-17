package lite

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// Series names for scope-drift menu observability. Each scenario can
// assert on these to distinguish the menu path (drift_mint > 0) from
// the proactive path (drift_mint == 0). Pre-clear consumption proves
// the agent retried the originally-blocked tool_use after the
// user-approved option resolved.
const (
	SeriesScopeDriftMint           = "scope_drift.mint"
	SeriesScopeDriftClaimExpand    = "scope_drift.claim.expand"
	SeriesScopeDriftClaimNewTask   = "scope_drift.claim.new_task"
	SeriesScopeDriftClaimOneOff    = "scope_drift.claim.one_off"
	SeriesScopeDriftOutcomeSucceed = "scope_drift.outcome.succeeded"
	SeriesScopeDriftOutcomeDenied  = "scope_drift.outcome.denied"
	SeriesScopeDriftPreClear       = "scope_drift.pre_clear_consumed"
	SeriesScopeDriftPendingMint    = "scope_drift.pending_substitution.minted"
	SeriesScopeDriftPendingConsume = "scope_drift.pending_substitution.consumed"
)

// countingScopeDriftRegistry wraps a real ScopeDriftRegistry and
// increments harness counters at each lifecycle transition. The
// wrapper only observes; it never changes outcomes.
type countingScopeDriftRegistry struct {
	inner    llmproxy.ScopeDriftRegistry
	counters *Counters
}

var _ llmproxy.ScopeDriftRegistry = (*countingScopeDriftRegistry)(nil)

func (c *countingScopeDriftRegistry) Register(ctx context.Context, drift llmproxy.ScopeDrift) (llmproxy.ScopeDrift, error) {
	stored, err := c.inner.Register(ctx, drift)
	if err == nil {
		c.counters.Inc(SeriesScopeDriftMint)
	}
	return stored, err
}

func (c *countingScopeDriftRegistry) Get(ctx context.Context, driftID string) (llmproxy.ScopeDrift, error) {
	return c.inner.Get(ctx, driftID)
}

func (c *countingScopeDriftRegistry) ClaimOption(ctx context.Context, driftID string, option llmproxy.ScopeDriftOption, agentNote string) (llmproxy.ScopeDrift, error) {
	claimed, err := c.inner.ClaimOption(ctx, driftID, option, agentNote)
	if err == nil {
		switch option {
		case llmproxy.ScopeDriftOptionExpand:
			c.counters.Inc(SeriesScopeDriftClaimExpand)
		case llmproxy.ScopeDriftOptionNewTask:
			c.counters.Inc(SeriesScopeDriftClaimNewTask)
		case llmproxy.ScopeDriftOptionOneOff:
			c.counters.Inc(SeriesScopeDriftClaimOneOff)
		}
	}
	return claimed, err
}

func (c *countingScopeDriftRegistry) SetOutcome(ctx context.Context, driftID string, outcome llmproxy.ScopeDriftOutcome) error {
	if err := c.inner.SetOutcome(ctx, driftID, outcome); err != nil {
		return err
	}
	switch outcome {
	case llmproxy.ScopeDriftOutcomeSucceeded:
		c.counters.Inc(SeriesScopeDriftOutcomeSucceed)
	case llmproxy.ScopeDriftOutcomeDenied:
		c.counters.Inc(SeriesScopeDriftOutcomeDenied)
	}
	return nil
}

func (c *countingScopeDriftRegistry) LookupPreClear(ctx context.Context, agentID, fingerprint string) (string, bool) {
	driftID, ok := c.inner.LookupPreClear(ctx, agentID, fingerprint)
	if ok {
		c.counters.Inc(SeriesScopeDriftPreClear)
	}
	return driftID, ok
}

func (c *countingScopeDriftRegistry) RegisterPendingSubstitution(ctx context.Context, key llmproxy.PendingSubstitutionKey, value llmproxy.PendingSubstitution) error {
	if err := c.inner.RegisterPendingSubstitution(ctx, key, value); err != nil {
		return err
	}
	c.counters.Inc(SeriesScopeDriftPendingMint)
	return nil
}

func (c *countingScopeDriftRegistry) LookupPendingSubstitution(ctx context.Context, key llmproxy.PendingSubstitutionKey) (llmproxy.PendingSubstitution, bool) {
	value, ok := c.inner.LookupPendingSubstitution(ctx, key)
	if ok {
		c.counters.Inc(SeriesScopeDriftPendingConsume)
	}
	return value, ok
}

func (c *countingScopeDriftRegistry) DeletePendingSubstitution(ctx context.Context, key llmproxy.PendingSubstitutionKey) {
	c.inner.DeletePendingSubstitution(ctx, key)
}
