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
	// Continue is set if any evaluator returned a ContinueSignal.
	// Continuation re-enters the pipeline with the synthetic body.
	Continue *ContinueSignal
	// ContinueFromToolUseID is the ID of the tool_use that triggered
	// continuation. Useful for audit forensics.
	ContinueFromToolUseID string
}

// ToolUseEvaluation captures one (evaluator, tool_use) pair's verdict.
type ToolUseEvaluation struct {
	EvaluatorName string
	ToolUseID     string
	Verdict       ToolUseVerdict
}

// EvaluateToolUses runs every evaluator in declared order against
// every tool_use in toolUses. For each tool_use, the first evaluator
// returning Outcome != Skip wins; later evaluators don't run for that
// tool_use. Per-tool-use mutations queue on the supplied ToolUseMutator
// (one mutator per tool_use, constructed by the caller).
//
// Continuation: a Continue signal on any verdict short-circuits the
// whole pass — coalescing-of-Continues isn't a thing (continuation is
// always single-tool-use by construction). The orchestrator records
// the trigger ID and returns; the caller re-enters the pipeline.
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

	for _, tu := range toolUses {
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
			if verdict.Continue != nil {
				if verdict.Outcome != OutcomeAllow && verdict.Outcome != OutcomeRewrite {
					return nil, fmt.Errorf("evaluator %q on tool_use %q returned Continue with outcome %q; Continue requires Allow or Rewrite", ev.Name(), tu.ID, verdict.Outcome)
				}
				// Continuation short-circuits the whole pass.
				result.Continue = verdict.Continue
				result.ContinueFromToolUseID = tu.ID
				result.PerToolUse[tu.ID] = verdict
				return result, nil
			}
			if verdict.Outcome != OutcomeSkip {
				v := verdict
				winner = &v
				break
			}
		}
		if winner == nil {
			// No evaluator claimed this tool_use — default to Allow.
			// The inspector chain has its own fail-closed rules that
			// emit Deny via the evaluator, not by absence.
			result.PerToolUse[tu.ID] = ToolUseVerdict{Outcome: OutcomeAllow}
		} else {
			result.PerToolUse[tu.ID] = *winner
		}
	}
	return result, nil
}
