package postproc

// Test helpers for postproc tests. Internal-test files use
// pipelineFactory to populate PostprocessConfig.ToolUseEvaluatorFactory
// without each test importing pipelineeval directly.

import (
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipelineeval"
)

// pipelineFactory is the production tool_use evaluator factory.
// Tests assign it to PostprocessConfig.ToolUseEvaluatorFactory.
var pipelineFactory llmproxy.ToolUseEvaluatorFactory = pipelineeval.Factory
