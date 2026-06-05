package pipeline

// CoalescedHold groups sibling tool_uses whose Hold verdicts share a
// HoldKey. Phase 5's coalescing pass produces these from a
// ToolUseResult; downstream code emits one combined approval row per
// group rather than N individual ones.
type CoalescedHold struct {
	HoldKey    string
	ToolUseIDs []string
	// Verdicts is the per-tool verdict trail for the coalesced group.
	// Useful for audit (every tool_use that was Held under this key
	// gets its own audit row tagged with the shared approval).
	Verdicts []ToolUseVerdict
}

// CoalesceHolds groups the Hold verdicts in r by HoldKey. Verdicts
// without a HoldKey, or with non-Hold outcomes, are skipped — those
// produce per-tool approvals (or no approval at all) on their own.
//
// Returns groups in deterministic order based on the order tool_uses
// appear in r.PerToolUse iteration. (Since maps don't have stable
// iteration, the orchestrator uses r.Evaluations — which IS ordered —
// as the source.)
//
// A group of size 1 is still returned; the caller decides whether to
// treat single-tool Holds as "coalesced" (one approval) or
// "non-coalesced" (one approval anyway). The semantic is identical;
// the data structure is the only difference.
func CoalesceHolds(r *ToolUseResult) []CoalescedHold {
	if r == nil {
		return nil
	}

	// Walk Evaluations in order so output groups are deterministic.
	// Within each tool_use we want the winning verdict (the one stored
	// in PerToolUse). Skip evaluations that didn't end up the winner.
	byKey := map[string][]struct {
		toolUseID string
		verdict   ToolUseVerdict
	}{}
	seenTool := map[string]bool{}
	keysOrder := []string{}

	for _, ev := range r.Evaluations {
		if seenTool[ev.ToolUseID] {
			continue
		}
		winner, ok := r.PerToolUse[ev.ToolUseID]
		if !ok || winner.Outcome != OutcomeHold || winner.HoldKey == "" {
			continue
		}
		// Mark this tool_use as accounted-for to avoid re-adding it on
		// a later evaluation entry.
		seenTool[ev.ToolUseID] = true

		// Track first-seen order of keys.
		if _, ok := byKey[winner.HoldKey]; !ok {
			keysOrder = append(keysOrder, winner.HoldKey)
		}
		byKey[winner.HoldKey] = append(byKey[winner.HoldKey], struct {
			toolUseID string
			verdict   ToolUseVerdict
		}{toolUseID: ev.ToolUseID, verdict: winner})
	}

	out := make([]CoalescedHold, 0, len(keysOrder))
	for _, key := range keysOrder {
		entries := byKey[key]
		group := CoalescedHold{
			HoldKey:    key,
			ToolUseIDs: make([]string, 0, len(entries)),
			Verdicts:   make([]ToolUseVerdict, 0, len(entries)),
		}
		for _, e := range entries {
			group.ToolUseIDs = append(group.ToolUseIDs, e.toolUseID)
			group.Verdicts = append(group.Verdicts, e.verdict)
		}
		out = append(out, group)
	}
	return out
}
