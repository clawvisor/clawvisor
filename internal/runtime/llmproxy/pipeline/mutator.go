package pipeline

import "encoding/json"

// RequestMutator records mutations to apply to the inbound request body.
// The interface is a command queue, not an immediate edit: in the
// pre-phase, mutations apply eagerly so later policies see earlier
// edits.
//
// Methods should have contract tests scoped to the provider and body
// shape they mutate.
type RequestMutator interface {
	// ReplaceBody swaps the entire request body bytes. Used by
	// transformations that produce a whole-body output (e.g.,
	// anthropic_sanitize). Later policies see the replaced body through
	// the ReadOnlyRequest passed to their own Evaluate call. A saved
	// earlier ReadOnlyRequest remains immutable.
	// Returns an error if the new body fails the per-provider parse
	// check.
	ReplaceBody(newBody []byte) error

	// InjectSystemNotice appends to the system prompt for both providers.
	InjectSystemNotice(text string) error

	// PrependUserTurn inserts a synthetic user turn at the start of the
	// conversation (e.g., Clawvisor outcome notices that the LLM should
	// see as user context).
	PrependUserTurn(text string) error

	// RewriteHistoricalToolUseArgs replaces the input of an existing
	// assistant tool_use in conversation history. Used by inbound_sanitize
	// to strip proxy rewrite artifacts before the model sees its own
	// history.
	RewriteHistoricalToolUseArgs(toolUseID string, newInput json.RawMessage) error

	// StripTurns drops conversation turns matching the predicate. Used
	// by synthetic_history_strip and secret_history_strip.
	StripTurns(predicate func(turn StripContext) bool) error

	// RewriteMostRecentUserText replaces the text of the latest user
	// turn (e.g., "approve" → rendered outcome). Implementation must
	// handle string-vs-array content shapes per provider.
	RewriteMostRecentUserText(newText string) error

	// RedactSpans erases byte ranges in the raw request body. Used by
	// secret_detection to redact found secrets without re-parsing.
	RedactSpans(spans []ByteSpan) error

	// AppendContinuationTurn appends the synthesized assistant+tool_result
	// pair that closes a local interception. Used for continuation
	// re-entry.
	AppendContinuationTurn(synth SyntheticContinuation) error
}

// StripContext is the predicate input for RequestMutator.StripTurns. It
// carries enough to identify a turn (role + content fingerprint) without
// exposing the full provider-specific shape to the predicate.
type StripContext struct {
	Role       string // "user" | "assistant" | "tool" | "system"
	Index      int    // position in the conversation
	TextSample string // first ~200 chars of text content, for pattern matching
}

// ResponseMutator records mutations to the outbound response. Unlike
// RequestMutator, the post-phase queues mutations and commits them
// once at end-of-phase (after the coalesce phase has merged Holds).
//
// The streaming and buffered paths both go through the same mutator;
// the encoder underneath handles framing.
type ResponseMutator interface {
	// PrependAssistantText injects a leading text block in the assistant
	// turn.
	PrependAssistantText(text string) error

	// SubstituteEntireResponse replaces the entire assistant response
	// with synthesized text. Used by inline_task_intercept to swap the
	// model's POST /api/control/tasks tool_use with a human approval
	// prompt.
	SubstituteEntireResponse(text string) error
}

// ToolUseMutator scopes mutations to a single tool_use during evaluator
// fan-out. Isolating per-tool-use edits here (rather than allowing a
// ToolUseEvaluator to use ResponseMutator) preserves the coalescing
// invariant: sibling tool_uses must be independently rewritable
// without one evaluator's mutation accidentally clobbering another's.
type ToolUseMutator interface {
	// RewriteArgs replaces the tool_use input bytes. Used by
	// control_rewrite (nonce injection) and credential_rewrite (autovault).
	RewriteArgs(newInput json.RawMessage) error

	// ReplaceWithText synthesizes a tool_result for this tool_use with
	// the given text. Used when the proxy serves the tool call locally
	// (e.g., synthesized error responses on denial).
	ReplaceWithText(text string) error
}
