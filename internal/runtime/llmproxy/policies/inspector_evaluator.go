package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// InspectorEvaluator wraps the autovault Inspector.Inspect call as a
// ToolUseEvaluator. It's the entry point for the inspector chain that
// Phase 4 extracts from newToolUseEvaluator in postprocess.go.
//
// Outcomes:
//   - Inspector trigger miss (no autovault placeholder substring) → Skip.
//     Other evaluators may still claim this tool_use; default-Allow
//     handles the all-Skip case.
//   - Inspector verdict ambiguous → Hold with a single-tool HoldKey
//     (the existing system fails closed on ambiguous).
//   - Inspector recognizes the call as a credentialed API call → Allow
//     with the verdict surface available to subsequent evaluators via
//     AuditFields. Boundary check + intent verify chain on top.
//
// Today's newToolUseEvaluator runs the inspector, then the boundary
// check, then intent verify, then task scope, with mutations and audit
// rows interleaved. This evaluator carves out the first step;
// follow-ups in the chain handle the subsequent decisions.
type InspectorEvaluator struct {
	inspector *inspector.Inspector
}

// NewInspectorEvaluator constructs the evaluator. Nil inspector → Skip
// (matches today's "if h.Inspector == nil, no inspection" gate).
func NewInspectorEvaluator(insp *inspector.Inspector) *InspectorEvaluator {
	return &InspectorEvaluator{inspector: insp}
}

// Name returns the audit-friendly evaluator identifier.
func (InspectorEvaluator) Name() string { return "inspector" }

// Evaluate inspects the tool_use. The verdict's IsAPICall / Ambiguous
// / Placeholders fields are surfaced through AuditFields so later
// evaluators can branch without re-inspecting.
func (e *InspectorEvaluator) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if e.inspector == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	v := e.inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})

	fields := map[string]any{
		"inspector_source":   string(v.Source),
		"inspector_is_api":   v.IsAPICall,
	}
	if v.Reason != "" {
		fields["inspector_reason"] = v.Reason
	}
	if v.Method != "" {
		fields["inspector_method"] = v.Method
	}
	if v.Host != "" {
		fields["inspector_host"] = v.Host
	}
	if v.Path != "" {
		fields["inspector_path"] = v.Path
	}
	if len(v.Placeholders) > 0 {
		fields["inspector_placeholders"] = v.Placeholders
	}

	// Ambiguous verdict → Hold per-tool. The legacy code fails closed
	// here; the policy preserves that. HoldKey is per-tool (no
	// coalescing across siblings) because ambiguous-different-reasons
	// shouldn't merge into one approval prompt.
	if v.Ambiguous {
		return pipeline.ToolUseVerdict{
			Outcome:     pipeline.OutcomeHold,
			Reason:      v.Reason,
			AuditFields: fields,
			HoldKey:     "ambiguous_" + tu.ID,
		}, nil
	}

	// Trigger miss → Skip, let downstream evaluators decide.
	if v.Source == inspector.SourceTriggerMiss {
		return pipeline.ToolUseVerdict{
			Outcome:     pipeline.OutcomeSkip,
			AuditFields: fields,
		}, nil
	}

	// Recognized API call → Allow, surface verdict via AuditFields.
	return pipeline.ToolUseVerdict{
		Outcome:     pipeline.OutcomeAllow,
		AuditFields: fields,
	}, nil
}

var _ pipeline.ToolUseEvaluator = (*InspectorEvaluator)(nil)
