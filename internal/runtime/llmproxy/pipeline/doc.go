// Package pipeline defines the policy/mutator interfaces that the LLM
// proxy will be carved into. See .context/llmproxy-refactor-plan.md
// (Phase 1) for context.
//
// THE INTERFACES IN THIS PACKAGE ARE DESIGN HYPOTHESES UNTIL THE FIRST
// CONCRETE POLICY MIGRATION VALIDATES THEM. Mutator implementations panic
// if called — nothing wires through this package until Phase 2 ports the
// first operation (PrependAssistantText via the new event stream).
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
// The mutator surface will grow as policies migrate. Each new method
// arrives with a contract test scoped to that method's per-provider,
// per-stream-shape behavior — not pre-emptively for unused operations.
package pipeline
