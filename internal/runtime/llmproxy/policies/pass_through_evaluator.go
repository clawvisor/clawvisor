package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// PassThroughEvaluator is the explicit allow tail for unclaimed
// non-credentialed tool_uses. Keeping this as a real evaluator makes
// pipeline.EvaluateToolUses fail closed when a caller forgets the tail.
type PassThroughEvaluator struct{}

func NewPassThroughEvaluator() *PassThroughEvaluator { return &PassThroughEvaluator{} }

func (PassThroughEvaluator) Name() string { return "pass_through" }

func (PassThroughEvaluator) Evaluate(context.Context, pipeline.ReadOnlyResponse, conversation.ToolUse, pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeAllow}, nil
}

var _ pipeline.ToolUseEvaluator = (*PassThroughEvaluator)(nil)
