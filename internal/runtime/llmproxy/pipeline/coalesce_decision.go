package pipeline

// ShouldCoalesce decides whether a ToolUseResult's Hold verdicts
// should be merged into a single combined approval. Mirrors the
// legacy shouldCoalesceTurn rule from coalesce.go:
//
//   - Need multiple tool_use verdicts (single-tool-use turns can't
//     coalesce).
//   - At least one Hold must be present (nothing to coalesce
//     otherwise).
//   - No inline-task hold present (those are single-tool by design
//     and confuse the UI when grouped with siblings).
//   - No hard Deny present (mixing "approve to run X, but Y is
//     permanently blocked" in one prompt is confusing UX).
//
// When true: the caller treats every Hold verdict in r.PerToolUse as
// part of one combined approval (use CoalesceHolds to produce the
// groups). When false: each Hold gets its own approval row as today's
// non-coalesced path does.
func ShouldCoalesce(r *ToolUseResult) bool {
	if r == nil {
		return false
	}
	if len(r.PerToolUse) <= 1 {
		return false
	}

	approvals := 0
	for _, v := range r.PerToolUse {
		switch v.Outcome {
		case OutcomeHold:
			// Inline-task holds shouldn't coalesce with sibling
			// approvals — the inline-task flow has its own UI shape.
			// We detect inline-task holds by their HoldKey prefix
			// (set by InlineTaskIntercept evaluators when they hold).
			if isInlineTaskHoldKey(v.HoldKey) {
				return false
			}
			approvals++
		case OutcomeDeny:
			// A hard deny in the turn breaks coalescing — the user
			// shouldn't be asked to approve siblings of a denied call.
			return false
		}
	}
	return approvals >= 1
}

// isInlineTaskHoldKey checks whether a HoldKey came from an inline-
// task hold rather than a standard tool-stage hold. The convention
// matches the legacy code: inline-task holds prefix their HoldKey
// (today's HeldKind=HeldKindApproval + Stage=StageAwaitingTaskApproval
// maps to this branch).
//
// Inline-task evaluators that emit Hold should use HoldKeys of the
// form "inline_task_<id>" to be recognized by this rule.
func isInlineTaskHoldKey(holdKey string) bool {
	const prefix = "inline_task_"
	return len(holdKey) > len(prefix) && holdKey[:len(prefix)] == prefix
}
