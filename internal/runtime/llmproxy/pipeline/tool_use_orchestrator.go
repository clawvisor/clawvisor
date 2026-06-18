package pipeline

import (
	"context"
	"fmt"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// ToolUseResult is what Pipeline.EvaluateToolUses returns after
// running every ToolUseEvaluator against every tool_use in the
// response. The orchestrator collects verdicts but does NOT commit
// mutations or audit rows — coalescing decisions need the full set of
// verdicts before any side effect lands.
type ToolUseResult struct {
	// PerToolUse maps tool_use ID to the verdict that won. Each tool_use
	// runs through every evaluator in declared order; the first
	// evaluator that returns Outcome != Skip wins.
	PerToolUse map[string]ToolUseVerdict
	// Evaluations is the full call trail (every tool_use × every
	// evaluator). Useful for telemetry / forensics; coalescing operates
	// on PerToolUse, not the trail.
	Evaluations []ToolUseEvaluation
}

// ToolUseEvaluation captures one (evaluator, tool_use) pair's verdict.
type ToolUseEvaluation struct {
	EvaluatorName string
	ToolUseID     string
	Verdict       ToolUseVerdict
	Winning       bool
}

// EvaluateToolUses runs every evaluator in declared order against
// every tool_use in toolUses. For each tool_use, the first evaluator
// returning Outcome != Skip wins; later evaluators don't run for that
// tool_use. Per-tool-use mutations queue on the supplied ToolUseMutator
// (one mutator per tool_use, constructed by the caller).
//
// Hold coalescing is handled after this pass by Finalizer, which needs
// the full sibling verdict set and the buffered hold captures.
func EvaluateToolUses(
	ctx context.Context,
	res ReadOnlyResponse,
	toolUses []conversation.ToolUse,
	evaluators []ToolUseEvaluator,
	mutatorFor func(toolUseID string) ToolUseMutator,
) (*ToolUseResult, error) {
	if res == nil {
		return nil, fmt.Errorf("pipeline.EvaluateToolUses: nil response")
	}
	if mutatorFor == nil {
		return nil, fmt.Errorf("pipeline.EvaluateToolUses: nil mutator factory")
	}

	result := &ToolUseResult{
		PerToolUse:  make(map[string]ToolUseVerdict, len(toolUses)),
		Evaluations: make([]ToolUseEvaluation, 0, len(toolUses)*len(evaluators)),
	}

	seenToolUseIDs := make(map[string]struct{}, len(toolUses))
	for _, tu := range toolUses {
		if _, ok := seenToolUseIDs[tu.ID]; ok {
			return nil, fmt.Errorf("pipeline.EvaluateToolUses: duplicate tool_use id %q", tu.ID)
		}
		seenToolUseIDs[tu.ID] = struct{}{}

		mut := mutatorFor(tu.ID)
		var winner *ToolUseVerdict
		for _, ev := range evaluators {
			verdict, err := ev.Evaluate(ctx, res, tu, mut)
			if err != nil {
				return nil, fmt.Errorf("evaluator %q on tool_use %q: %w", ev.Name(), tu.ID, err)
			}
			result.Evaluations = append(result.Evaluations, ToolUseEvaluation{
				EvaluatorName: ev.Name(),
				ToolUseID:     tu.ID,
				Verdict:       verdict,
			})
			if verdict.Outcome != OutcomeSkip {
				result.Evaluations[len(result.Evaluations)-1].Winning = true
				v := verdict
				winner = &v
				break
			}
		}
		if winner == nil {
			result.PerToolUse[tu.ID] = defaultUnclaimedToolUseVerdict(result.Evaluations, tu.ID)
		} else {
			result.PerToolUse[tu.ID] = *winner
		}
	}
	return result, nil
}

func defaultUnclaimedToolUseVerdict(evaluations []ToolUseEvaluation, toolUseID string) ToolUseVerdict {
	var credentialedFact *InspectorFact
	for _, ev := range evaluations {
		if ev.ToolUseID != toolUseID {
			continue
		}
		for _, fact := range ev.Verdict.Facts {
			switch inspectorFact := fact.(type) {
			case InspectorFact:
				if credentialedFact == nil && inspectorFact.IsAPICall {
					copyFact := inspectorFact
					credentialedFact = &copyFact
				}
			}
		}
	}
	if credentialedFact != nil {
		// Terminal — not recoverable. This fires when InspectorChain
		// classified the call as credentialed but no downstream
		// evaluator (script-session, control-tool, credential-rewrite)
		// claimed it. That's a policy-misconfiguration / fail-closed
		// scenario, not an agent construction error: there is no
		// alternate call shape the agent can try that would route
		// around a missing claimant, so flowing the reason back as a
		// recoverable continuation would just burn the one-retry
		// budget and produce a confusing UX. The credential_rewrite
		// evaluator's own rewriter-error path (which IS recoverable)
		// carries actionable "switch to a script session" guidance
		// for the cases where shape-changing actually helps.
		return ToolUseVerdict{
			Outcome: OutcomeDeny,
			Reason:  "Clawvisor: credentialed API call was not rewritten; refusing to send original tool_use upstream",
			Facts:   []EvaluationFact{*credentialedFact},
		}
	}
	return ToolUseVerdict{
		Outcome: OutcomeDeny,
		Reason:  "Clawvisor: no policy claimed this tool use; refusing to run it",
	}
}

