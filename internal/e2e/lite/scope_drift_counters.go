package lite

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// Series names tracking scope-drift lifecycle events. Surfaced as the
// menu/recovery contract changes: scenarios can assert the proxy minted
// a drift, the agent picked a specific option, the drift resolved
// positively or negatively, and the pre-clear actually fired on the
// agent's retry.
//
// All counts are incremented by countingScopeDriftRegistry, which wraps
// the in-memory registry production wires. The option-chosen counters
// fire on ClaimOption (markup-based options c/d). Options (a)/(b) reuse
// /control/tasks{,/expand} and don't currently round-trip through the
// drift registry — the existing approvals.allow_session and tasks.*
// counters cover them. Future work to bind drift_id into (a)/(b) calls
// would let us emit scope_drift.option_chosen.{expand,new_task} here.
const (
	SeriesScopeDriftMinted          = "scope_drift.minted"
	SeriesScopeDriftClaimedOneOff   = "scope_drift.option_chosen.one_off"
	SeriesScopeDriftClaimedJustify  = "scope_drift.option_chosen.justify"
	SeriesScopeDriftOutcomeOK       = "scope_drift.outcome.succeeded"
	SeriesScopeDriftOutcomeDenied   = "scope_drift.outcome.denied"
	SeriesScopeDriftOutcomeFellBack = "scope_drift.outcome.fell_back"
	SeriesScopeDriftPreClearUsed    = "scope_drift.pre_clear_consumed"
)

// countingScopeDriftRegistry forwards every operation to inner and
// increments named series on each lifecycle event. Used by the lite
// harness so scenarios can assert against scope-drift behavior.
type countingScopeDriftRegistry struct {
	inner    llmproxy.ScopeDriftRegistry
	counters *Counters
}

// NewCountingScopeDriftRegistry wraps inner with the series-counting
// decorator. A nil inner is a programming error — the harness always
// constructs a real memory registry first.
func NewCountingScopeDriftRegistry(inner llmproxy.ScopeDriftRegistry, counters *Counters) llmproxy.ScopeDriftRegistry {
	return &countingScopeDriftRegistry{inner: inner, counters: counters}
}

func (r *countingScopeDriftRegistry) Register(ctx context.Context, drift llmproxy.ScopeDrift) (llmproxy.ScopeDrift, error) {
	out, err := r.inner.Register(ctx, drift)
	if err == nil {
		r.counters.Inc(SeriesScopeDriftMinted)
	}
	return out, err
}

func (r *countingScopeDriftRegistry) Get(ctx context.Context, driftID string) (llmproxy.ScopeDrift, error) {
	return r.inner.Get(ctx, driftID)
}

func (r *countingScopeDriftRegistry) ClaimOption(ctx context.Context, driftID string, option llmproxy.ScopeDriftOption, agentNote, agentJustification string) (llmproxy.ScopeDrift, error) {
	out, err := r.inner.ClaimOption(ctx, driftID, option, agentNote, agentJustification)
	if err == nil {
		switch option {
		case llmproxy.ScopeDriftOptionOneOff:
			r.counters.Inc(SeriesScopeDriftClaimedOneOff)
		case llmproxy.ScopeDriftOptionJustify:
			r.counters.Inc(SeriesScopeDriftClaimedJustify)
		}
	}
	return out, err
}

func (r *countingScopeDriftRegistry) SetOutcome(ctx context.Context, driftID string, outcome llmproxy.ScopeDriftOutcome) error {
	err := r.inner.SetOutcome(ctx, driftID, outcome)
	if err == nil {
		switch outcome {
		case llmproxy.ScopeDriftOutcomeSucceeded:
			r.counters.Inc(SeriesScopeDriftOutcomeOK)
		case llmproxy.ScopeDriftOutcomeDenied:
			r.counters.Inc(SeriesScopeDriftOutcomeDenied)
		case llmproxy.ScopeDriftOutcomeFellBack:
			r.counters.Inc(SeriesScopeDriftOutcomeFellBack)
		}
	}
	return err
}

func (r *countingScopeDriftRegistry) LookupPreClear(ctx context.Context, agentID, fingerprint string) (string, bool) {
	driftID, ok := r.inner.LookupPreClear(ctx, agentID, fingerprint)
	if ok {
		r.counters.Inc(SeriesScopeDriftPreClearUsed)
	}
	return driftID, ok
}
