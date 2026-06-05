package llmproxy_test

// Blank-import pipelineeval so its init() registers Factory as
// llmproxy.DefaultToolUseEvaluatorFactory before any test in this
// package runs. The llmproxy internal tests (package llmproxy) can't
// import pipelineeval directly — that would form a cycle with
// pipelineeval → llmproxy. But this external test file lives in
// package llmproxy_test (separate import graph) and is linked into
// the same test binary as the internal tests, so the init() side
// effect lands before any TestXxx runs in either package.
import _ "github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipelineeval"
