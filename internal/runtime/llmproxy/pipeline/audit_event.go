package pipeline

import "github.com/clawvisor/clawvisor/internal/runtime/conversation"

// AuditEvent aliases conversation.AuditEvent — Phase 8/9 unified the
// typed per-tool-use audit record so both pipeline orchestrator output
// and postproc-side buffering share the same shape.
type AuditEvent = conversation.AuditEvent

// DecisionKind aliases conversation.DecisionKind.
type DecisionKind = conversation.DecisionKind

const (
	DecisionAllow   = conversation.DecisionAllow
	DecisionBlock   = conversation.DecisionBlock
	DecisionRewrite = conversation.DecisionRewrite
)

// DecisionFromOutcome maps a pipeline Outcome to the coarse Decision
// the audit store expects.
func DecisionFromOutcome(o Outcome) DecisionKind {
	return conversation.DecisionFromOutcome(o)
}

// AuditEvents builds the typed audit-event stream for this result.
// Each (tool_use × evaluator) entry in Evaluations becomes one
// AuditEvent. The Winning flag identifies the verdict that claimed
// the tool_use; consumers needing only the final row filter on it.
//
// Caller supplies the toolUses slice so the event's ToolUse field
// carries the full block (the orchestrator's Evaluations[] entries
// reference tool_uses by ID only). When a tool_use isn't in the
// slice it's emitted with an empty ToolUse but the EvaluatorName +
// ToolUseID-on-the-trail still resolve via Evaluations.
func (r *ToolUseResult) AuditEvents(toolUses []conversation.ToolUse) []AuditEvent {
	if r == nil {
		return nil
	}
	byID := make(map[string]conversation.ToolUse, len(toolUses))
	for _, tu := range toolUses {
		byID[tu.ID] = tu
	}
	winner := make(map[string]string, len(toolUses))
	for _, ev := range r.Evaluations {
		if ev.Verdict.Outcome == OutcomeSkip {
			continue
		}
		if _, already := winner[ev.ToolUseID]; already {
			continue
		}
		winner[ev.ToolUseID] = ev.EvaluatorName
	}
	// inScope filters Evaluations to tool_uses the caller is interested
	// in. nil toolUses (not zero-length slice) signals "no filter";
	// callers wanting to suppress all rows pass an empty slice.
	inScope := func(tuID string) bool {
		if toolUses == nil {
			return true
		}
		_, ok := byID[tuID]
		return ok
	}
	events := make([]AuditEvent, 0, len(r.Evaluations))
	seenWinner := make(map[string]bool, len(toolUses))
	for _, ev := range r.Evaluations {
		if !inScope(ev.ToolUseID) {
			continue
		}
		isWinning := winner[ev.ToolUseID] == ev.EvaluatorName
		events = append(events, AuditEvent{
			ToolUse:       byID[ev.ToolUseID],
			EvaluatorName: ev.EvaluatorName,
			Outcome:       ev.Verdict.Outcome,
			Decision:      DecisionFromOutcome(ev.Verdict.Outcome),
			Reason:        ev.Verdict.Reason,
			Facts:         ev.Verdict.Facts,
			Winning:       isWinning,
		})
		if isWinning {
			seenWinner[ev.ToolUseID] = true
		}
	}
	// Synthesize winning events for tool_uses present in PerToolUse but
	// missing from Evaluations (test harnesses that construct result by
	// hand, or future paths that populate PerToolUse without trail
	// detail). The synthesized event has empty EvaluatorName but carries
	// the verdict's Outcome, Reason, and Facts so downstream emitters
	// still get a usable row.
	for tuID, v := range r.PerToolUse {
		if !inScope(tuID) || seenWinner[tuID] {
			continue
		}
		events = append(events, AuditEvent{
			ToolUse:  byID[tuID],
			Outcome:  v.Outcome,
			Decision: DecisionFromOutcome(v.Outcome),
			Reason:   v.Reason,
			Facts:    v.Facts,
			Winning:  true,
		})
	}
	return events
}
