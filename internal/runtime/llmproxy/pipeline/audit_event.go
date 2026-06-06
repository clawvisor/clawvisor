package pipeline

import "github.com/clawvisor/clawvisor/internal/runtime/conversation"

// AuditEvent is the typed per-tool-use audit record the pipeline
// produces from each Evaluations[] entry. Emitters (policies-side
// + handler-side) consume this instead of reading AuditFields keys
// or branching on Outcome enums directly.
//
// CRITICAL: AuditEvent is store-independent. It carries pipeline-domain
// primitives (Outcome, DecisionKind, the typed Facts) and lives in the
// pipeline package. Translation to store.AuditEntry happens OUTSIDE
// this package, in the audit emitter layer. The pipeline shape isn't
// driven by what columns the audit store happens to persist today —
// it's driven by what evaluators observe.
type AuditEvent struct {
	// ToolUse is the assistant tool_use block the verdict applies to.
	ToolUse conversation.ToolUse
	// EvaluatorName is the policy that produced the verdict (e.g.,
	// "inspector_chain", "task_scope", "credential_rewrite").
	EvaluatorName string
	// Outcome is the evaluator's verdict outcome.
	Outcome Outcome
	// Decision is the coarse-grained classification for downstream
	// stores that don't model the full Outcome enum. Translates from
	// Outcome via DecisionFromOutcome.
	Decision DecisionKind
	// Reason is the human-readable explanation.
	Reason string
	// Facts is the typed observation set the evaluator emitted.
	// Audit emitters branch via type switch over Facts to populate
	// stage-specific columns.
	Facts []EvaluationFact
	// Winning reports whether this event corresponds to the verdict
	// that won the tool_use's evaluation (first non-Skip in the chain).
	// Skip-and-still-observing evaluators emit AuditEvents with
	// Winning=false; downstream consumers filter as needed.
	Winning bool
}

// DecisionKind is the coarse audit-row classification, matching the
// legacy three-value enum the audit store uses.
type DecisionKind string

const (
	DecisionAllow   DecisionKind = "allow"
	DecisionBlock   DecisionKind = "block"
	DecisionRewrite DecisionKind = "rewrite"
)

// DecisionFromOutcome maps a pipeline Outcome to the coarse Decision
// the audit store expects. Hold and Deny both collapse to "block"
// because the user sees a refusal/approval prompt for either.
func DecisionFromOutcome(o Outcome) DecisionKind {
	switch o {
	case OutcomeAllow:
		return DecisionAllow
	case OutcomeRewrite:
		return DecisionRewrite
	case OutcomeDeny, OutcomeHold:
		return DecisionBlock
	default:
		// Skip / ShortCircuit shouldn't be audit-emitted as a final
		// verdict; default to Allow so accidental flows don't false-alarm.
		return DecisionAllow
	}
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
