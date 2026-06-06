package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// InspectorEvaluator wraps the autovault Inspector.Inspect call as a
// ToolUseEvaluator. It's the entry point for the inspector chain.
//
// Outcomes:
//   - Inspector trigger miss (no autovault placeholder substring) → Skip.
//     Other evaluators may still claim this tool_use; default-Allow
//     handles the all-Skip case.
//   - Inspector verdict ambiguous → Hold with a single-tool HoldKey
//     (the existing system fails closed on ambiguous).
//   - Inspector recognizes the call as a credentialed API call → Allow
//     with the verdict surface available to subsequent evaluators via
//     AuditParams. Boundary check + intent verify chain on top.
//
// This evaluator records the inspector observation; follow-ups in the
// chain handle boundary, intent, task-scope, authorization, and rewrite
// decisions.
type InspectorEvaluator struct {
	inspector *inspector.Inspector
}

// NewInspectorEvaluator constructs the evaluator. Nil inspector returns
// Skip, which lets the all-skip default allow behavior handle the call.
func NewInspectorEvaluator(insp *inspector.Inspector) *InspectorEvaluator {
	return &InspectorEvaluator{inspector: insp}
}

// Name returns the audit-friendly evaluator identifier.
func (InspectorEvaluator) Name() string { return "inspector" }

// Evaluate inspects the tool_use. Emits an InspectorFact carrying the
// verdict's IsAPICall / Ambiguous / Placeholders so later evaluators
// can branch without re-inspecting.
func (e *InspectorEvaluator) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if e.inspector == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	v := e.inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})
	fact := newInspectorFact(v)

	// Ambiguous verdict → Hold per-tool. The legacy code fails closed
	// here; the policy preserves that. HoldKey is per-tool (no
	// coalescing across siblings) because ambiguous-different-reasons
	// shouldn't merge into one approval prompt.
	if v.Ambiguous {
		return pipeline.ToolUseVerdict{
			Outcome:      pipeline.OutcomeHold,
			Reason:       v.Reason,
			HoldKey:      "ambiguous_" + tu.ID,
			HeldKindHint: pipeline.HeldKindHintApproval,
			Facts:        []pipeline.EvaluationFact{fact},
		}, nil
	}

	// Trigger miss → Skip, let downstream evaluators decide.
	if v.Source == inspector.SourceTriggerMiss {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeSkip,
			Facts:   []pipeline.EvaluationFact{fact},
		}, nil
	}

	// Recognized API call → Allow, surface verdict via the InspectorFact.
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeAllow,
		Facts:   []pipeline.EvaluationFact{fact},
	}, nil
}

var _ pipeline.ToolUseEvaluator = (*InspectorEvaluator)(nil)
