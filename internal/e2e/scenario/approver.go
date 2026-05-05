package scenario

import (
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	runtimeproxy "github.com/clawvisor/clawvisor/pkg/runtime/proxy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Decider is the interface the approver role expects. It mirrors
// roles.ApprovalDecider but is restated here so this package can return
// it from a single factory without a circular import.
type Decider interface {
	Decide(rec *store.ApprovalRecord, payload runtimeproxy.RuntimeApprovalPayload) string
}

// NewDecider picks the right decider for an Approvals block. Empty or
// "scripted" → ScriptedDecider; "probabilistic" → ProbabilisticDecider.
func NewDecider(approvals Approvals) Decider {
	if strings.EqualFold(strings.TrimSpace(approvals.Policy), "probabilistic") {
		return NewProbabilisticDecider(approvals)
	}
	return NewScriptedDecider(approvals)
}

// ScriptedDecider implements roles.ApprovalDecider for a scripted Approvals
// block. The first ApprovalRule whose match predicate is satisfied wins.
// If no rule matches, the block's Default is used; an empty Default means
// "deny" — fail closed so a missing rule never silently approves.
type ScriptedDecider struct {
	Rules   []ApprovalRule
	Default string
}

// NewScriptedDecider builds a decider from an Approvals block.
func NewScriptedDecider(approvals Approvals) *ScriptedDecider {
	def := strings.TrimSpace(approvals.Default)
	if def == "" {
		def = "deny"
	}
	return &ScriptedDecider{Rules: approvals.Rules, Default: def}
}

// Decide picks a resolution for one approval record.
func (d *ScriptedDecider) Decide(rec *store.ApprovalRecord, payload runtimeproxy.RuntimeApprovalPayload) string {
	for _, rule := range d.Rules {
		if matchApproval(rule.Match, rec, payload) {
			return rule.Resolution
		}
	}
	return d.Default
}

// ProbabilisticDecider samples a resolution from a per-rule weighted
// distribution. Rules are matched in order; the first matching rule's
// Weights map is sampled. Empty Weights falls back to the rule's static
// Resolution. No matching rule → Default ("deny" if blank). The same
// Seed produces the same sequence, so a flaky run is reproducible.
type ProbabilisticDecider struct {
	Rules   []ApprovalRule
	Default string

	mu  sync.Mutex
	rng *rand.Rand
}

// NewProbabilisticDecider builds a probabilistic decider from an Approvals
// block. Seed=0 means time-based (non-deterministic between runs).
func NewProbabilisticDecider(approvals Approvals) *ProbabilisticDecider {
	def := strings.TrimSpace(approvals.Default)
	if def == "" {
		def = "deny"
	}
	seed := approvals.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	return &ProbabilisticDecider{
		Rules:   approvals.Rules,
		Default: def,
		rng:     rand.New(rand.NewSource(seed)),
	}
}

// Decide picks a resolution for one approval record.
func (d *ProbabilisticDecider) Decide(rec *store.ApprovalRecord, p runtimeproxy.RuntimeApprovalPayload) string {
	for _, rule := range d.Rules {
		if !matchApproval(rule.Match, rec, p) {
			continue
		}
		if len(rule.Weights) == 0 {
			return rule.Resolution
		}
		d.mu.Lock()
		verb := sampleWeighted(d.rng, rule.Weights)
		d.mu.Unlock()
		if verb == "" {
			return rule.Resolution
		}
		return verb
	}
	return d.Default
}

// sampleWeighted draws one key from weights with probability proportional
// to its value. Keys are visited in sorted order so the seed pins the
// output across runs even though Go map iteration is randomized.
func sampleWeighted(rng *rand.Rand, weights map[string]float64) string {
	keys := make([]string, 0, len(weights))
	total := 0.0
	for k, v := range weights {
		if v <= 0 {
			continue
		}
		keys = append(keys, k)
		total += v
	}
	if total <= 0 || len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	r := rng.Float64() * total
	cum := 0.0
	for _, k := range keys {
		cum += weights[k]
		if r < cum {
			return k
		}
	}
	return keys[len(keys)-1]
}

func matchApproval(m ApprovalMatch, rec *store.ApprovalRecord, p runtimeproxy.RuntimeApprovalPayload) bool {
	if m.Kind != "" {
		if rec == nil || !strings.EqualFold(m.Kind, rec.Kind) {
			return false
		}
	}
	if m.Host != "" && !strings.EqualFold(m.Host, p.Host) {
		return false
	}
	if m.Method != "" && !strings.EqualFold(m.Method, p.Method) {
		return false
	}
	if m.PathPrefix != "" && !strings.HasPrefix(p.Path, m.PathPrefix) {
		return false
	}
	return true
}
