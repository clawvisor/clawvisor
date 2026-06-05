package handlers

import (
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipelineeval"
)

// pipelineToolUseEvaluatorFactory is the llmproxy.ToolUseEvaluatorFactory
// the handler installs on PostprocessConfig. The implementation lives
// in the pipelineeval leaf package so llmproxy's own internal tests
// can install the same factory without re-introducing the policies →
// llmproxy import cycle.
var pipelineToolUseEvaluatorFactory llmproxy.ToolUseEvaluatorFactory = pipelineeval.Factory
