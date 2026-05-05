package scenario

import (
	"fmt"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// Snapshot is the post-run state the evaluator scores hard expectations
// against. The harness builds it after the responder finishes — pulling from
// the store, the upstream hit counter, and the responder transcript.
type Snapshot struct {
	Events     []*store.RuntimeEvent
	Approvals  []*store.ApprovalRecord
	Pending    []*store.ApprovalRecord
	ToolCalls  int
	FinalReply string
	// UpstreamHits counts requests to each fake upstream by hostname.
	UpstreamHits map[string]int
}

// Failure is one expectation that did not hold. Reason is human readable.
type Failure struct {
	Index  int
	Series string
	Reason string
}

// Evaluate runs every Hard expectation against snap and returns the failures.
// Soft expectations are scored separately by the LLM judge.
func Evaluate(exp Expectations, snap *Snapshot) []Failure {
	var fails []Failure
	for i, hard := range exp.Hard {
		if hard.Count != nil {
			if f, ok := evalCount(*hard.Count, snap); !ok {
				fails = append(fails, Failure{Index: i, Series: hard.Count.Series, Reason: f})
			}
			continue
		}
		if hard.EventHas != nil {
			if !matchEvent(*hard.EventHas, snap.Events) {
				fails = append(fails, Failure{Index: i, Reason: fmt.Sprintf("no event matched %+v", *hard.EventHas)})
			}
			continue
		}
		if hard.FinalAssistantContains != "" {
			if !strings.Contains(strings.ToLower(snap.FinalReply), strings.ToLower(hard.FinalAssistantContains)) {
				fails = append(fails, Failure{Index: i, Reason: fmt.Sprintf("final reply does not contain %q", hard.FinalAssistantContains)})
			}
			continue
		}
		fails = append(fails, Failure{Index: i, Reason: "empty hard expectation"})
	}
	return fails
}

func evalCount(c CountExpect, snap *Snapshot) (string, bool) {
	value, err := resolveSeries(c.Series, snap)
	if err != nil {
		return err.Error(), false
	}
	if c.GTE != nil && value < *c.GTE {
		return fmt.Sprintf("series %s = %d, want >= %d", c.Series, value, *c.GTE), false
	}
	if c.LTE != nil && value > *c.LTE {
		return fmt.Sprintf("series %s = %d, want <= %d", c.Series, value, *c.LTE), false
	}
	if c.EQ != nil && value != *c.EQ {
		return fmt.Sprintf("series %s = %d, want == %d", c.Series, value, *c.EQ), false
	}
	return "", true
}

func resolveSeries(series string, snap *Snapshot) (int, error) {
	switch series {
	case "events.total":
		return len(snap.Events), nil
	case "events.allow":
		return countEvents(snap.Events, func(e *store.RuntimeEvent) bool { return strPtrEq(e.Decision, "allow") }), nil
	case "events.deny":
		return countEvents(snap.Events, func(e *store.RuntimeEvent) bool { return strPtrEq(e.Decision, "deny") }), nil
	case "events.review":
		return countEvents(snap.Events, func(e *store.RuntimeEvent) bool { return strPtrEq(e.Decision, "review") }), nil
	case "approvals.resolved":
		return countApprovals(snap.Approvals, func(a *store.ApprovalRecord) bool { return a.ResolvedAt != nil }), nil
	case "approvals.allow_once":
		return countApprovals(snap.Approvals, func(a *store.ApprovalRecord) bool { return a.Resolution == "allow_once" }), nil
	case "approvals.allow_session":
		return countApprovals(snap.Approvals, func(a *store.ApprovalRecord) bool { return a.Resolution == "allow_session" }), nil
	case "approvals.allow_always":
		return countApprovals(snap.Approvals, func(a *store.ApprovalRecord) bool { return a.Resolution == "allow_always" }), nil
	case "approvals.deny":
		return countApprovals(snap.Approvals, func(a *store.ApprovalRecord) bool { return a.Resolution == "deny" }), nil
	case "approvals.pending":
		return len(snap.Pending), nil
	case "tool_calls":
		return snap.ToolCalls, nil
	}
	if rest, ok := strings.CutPrefix(series, "upstream."); ok {
		if host, ok := strings.CutSuffix(rest, ".hits"); ok && host != "" {
			return snap.UpstreamHits[host], nil
		}
	}
	return 0, fmt.Errorf("unknown series %q", series)
}

func matchEvent(want EventExpect, events []*store.RuntimeEvent) bool {
	for _, e := range events {
		if want.EventType != "" && e.EventType != want.EventType {
			continue
		}
		if want.Decision != "" && !strPtrEq(e.Decision, want.Decision) {
			continue
		}
		if want.Outcome != "" && !strPtrEq(e.Outcome, want.Outcome) {
			continue
		}
		return true
	}
	return false
}

func countEvents(events []*store.RuntimeEvent, pred func(*store.RuntimeEvent) bool) int {
	n := 0
	for _, e := range events {
		if pred(e) {
			n++
		}
	}
	return n
}

func countApprovals(approvals []*store.ApprovalRecord, pred func(*store.ApprovalRecord) bool) int {
	n := 0
	for _, a := range approvals {
		if pred(a) {
			n++
		}
	}
	return n
}

func strPtrEq(p *string, want string) bool {
	return p != nil && *p == want
}
