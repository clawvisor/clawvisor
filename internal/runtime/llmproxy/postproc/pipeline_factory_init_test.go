package postproc_test

// Blank-import pipelineeval so its init() registers
// llmproxy.DefaultToolUseEvaluatorFactory before any test in the
// postproc binary runs. Mirrors the equivalent file in the llmproxy
// package — postproc tests use package postproc (internal) and can't
// reach pipelineeval without forming a cycle, so we add this
// external-test file to the same binary.
import _ "github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipelineeval"
