// Package pipeline defines the policy, mutator, evaluator, and
// finalizer interfaces that coordinate the LLM proxy's request,
// response, and tool_use flows.
//
// Three policy kinds carry different cardinality:
//
//   - RequestPolicy:    one verdict per inbound request
//   - ResponsePolicy:   one verdict per outbound response
//   - ToolUseEvaluator: one verdict per assistant tool_use in a response
//
// Mutations are commands, not data: policies invoke methods on the
// mutator interfaces; the orchestrator collects mutations and commits
// them at end-of-phase. This is what makes coalescing tractable — the
// orchestrator can merge multiple Hold verdicts sharing a HoldKey into
// one combined approval before any audit row or cache write happens.
//
// Each mutator method should have contract coverage scoped to that
// method's per-provider, per-stream-shape behavior.
package pipeline
