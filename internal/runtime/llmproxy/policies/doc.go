// Package policies hosts the individual RequestPolicy / ResponsePolicy /
// ToolUseEvaluator implementations that the LLM proxy pipeline runs.
//
// Each policy lives in its own file, takes its dependencies (stores,
// caches, verifiers, etc.) via its constructor, and implements only
// the interface kind it needs (most policies are RequestPolicy or
// ResponsePolicy; the inspector chain composes into a ToolUseEvaluator).
//
// Migration order follows §5 / §6 of the refactor plan: simpler
// transformations first, the inspector chain and inline-task
// orchestration last. The first policy to land is anthropic_sanitize
// — pure body transformation, no shared state.
package policies
