package pipeline

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// Outcome is the coarse verdict category a policy returns. Audit rows
// already use these labels; the names match for symmetry.
type Outcome string

const (
	// OutcomeAllow — request/response continues unchanged through the
	// remaining policies. The policy may still have queued mutations.
	OutcomeAllow Outcome = "allow"
	// OutcomeDeny — terminate the pipeline at this policy. Subsequent
	// policies do not run. Audit records the deny.
	OutcomeDeny Outcome = "deny"
	// OutcomeHold — the tool_use needs human approval before proceeding.
	// Only valid on ToolUseVerdict. The orchestrator coalesces Holds
	// sharing a HoldKey into one combined approval.
	OutcomeHold Outcome = "hold"
	// OutcomeRewrite — the tool_use args have been rewritten via the
	// ToolUseMutator; the response continues with the new args in place.
	// Only valid on ToolUseVerdict.
	OutcomeRewrite Outcome = "rewrite"
	// OutcomeShortCircuit — skip remaining preprocess policies AND the
	// upstream forward step; return the SyntheticResponse to the client.
	// Only valid on RequestVerdict.
	OutcomeShortCircuit Outcome = "short_circuit"
	// OutcomeSkip — this policy declined to act on this request/response
	// (e.g., agent_notice only runs on the first turn). No audit row,
	// no mutations.
	OutcomeSkip Outcome = "skip"
)

// RequestPolicy runs once per inbound request and emits exactly one verdict.
// Examples: control_notice injection, secret_detection, agent_notice
// (preprocess half), inbound_sanitize.
type RequestPolicy interface {
	Name() string
	Preprocess(ctx context.Context, req ReadOnlyRequest, mut RequestMutator) (RequestVerdict, error)
}

// ResponsePolicy runs once per outbound response and emits exactly one
// verdict. Examples: agent_notice (postprocess: prepend notice),
// inline_task_intercept (substitute response with approval prompt).
type ResponsePolicy interface {
	Name() string
	Postprocess(ctx context.Context, res ReadOnlyResponse, mut ResponseMutator) (ResponseVerdict, error)
}

// ToolUseEvaluator runs once per assistant tool_use in a response and
// emits one verdict per tool_use. The orchestrator collects every
// verdict before committing any mutation — that's what enables
// coalescing of multiple Holds.
//
// The inspector chain (boundary check → intent verify → task scope →
// control_rewrite) composes into a single ToolUseEvaluator chain;
// today's newToolUseEvaluator closure in postprocess.go is exactly
// this shape.
type ToolUseEvaluator interface {
	Name() string
	Evaluate(ctx context.Context, res ReadOnlyResponse, tu conversation.ToolUse, mut ToolUseMutator) (ToolUseVerdict, error)
}

// ReadOnlyRequest exposes the parsed, lossy view of the inbound request.
// Mutations never go through here — they go through RequestMutator.
//
// First-turn detection and conversation-ID minting happen *upstream* of
// policies, so policies see stable values. (Today the handler interleaves
// minting with policy execution; the refactor lifts that to before-policy
// initialization.)
type ReadOnlyRequest interface {
	Provider() conversation.Provider
	StreamShape() conversation.StreamShape
	Turns() []conversation.Turn
	HTTPRequest() *http.Request
	RawBody() []byte
	IsFirstTurn() bool
	ConversationID() string
	// UserID is the authenticated user owning this request. Required
	// by policies that scope state by user (inline-approval outcomes,
	// secret decisions, vault lookups). Empty if unauthenticated —
	// though the proxy refuses unauthenticated requests upstream of
	// the pipeline.
	UserID() string
	// AgentID is the authenticated agent. Same scoping shape as
	// UserID; empty when the request is user-scoped only.
	AgentID() string
}

// ReadOnlyResponse exposes the response under inspection.
//
// For buffered responses, ToolUses() returns the full set immediately.
// For streaming responses, the orchestrator populates this incrementally
// as block_end events arrive — ToolUseEvaluators run per-tool_use as
// each one completes.
type ReadOnlyResponse interface {
	Provider() conversation.Provider
	StreamShape() conversation.StreamShape
	IsStreaming() bool
	ToolUses() []conversation.ToolUse
}

// RequestVerdict is the result of a RequestPolicy.Preprocess call.
type RequestVerdict struct {
	Outcome     Outcome
	Reason      string
	AuditFields map[string]any
	// ShortCircuit is set when Outcome == ShortCircuit. The pipeline
	// skips remaining pre policies AND the forward step; the synthetic
	// body enters the post-phase as if it were an upstream response.
	ShortCircuit *SyntheticResponse
}

// ResponseVerdict is the result of a ResponsePolicy.Postprocess call.
type ResponseVerdict struct {
	Outcome     Outcome
	Reason      string
	AuditFields map[string]any
}

// ToolUseVerdict is the result of a ToolUseEvaluator.Evaluate call for
// one tool_use. Multiple Holds with the same HoldKey coalesce into one
// combined approval.
type ToolUseVerdict struct {
	Outcome     Outcome
	Reason      string
	AuditFields map[string]any
	// HoldKey groups sibling tool_uses for coalescing. Empty means
	// "do not coalesce" (each Hold gets its own approval row).
	HoldKey string
	// Continue lifts continuation out of "mutation" into a control-flow
	// signal. When set, the tool_use is being served locally and the
	// pipeline re-enters with the synthetic continuation as the next
	// request.
	Continue *ContinueSignal
	// ContinueWithToolResultText, when non-empty, surfaces as
	// conversation.ToolUseVerdict.ContinueWithToolResult via the
	// bridge. Used for refusal paths where the evaluator wants the
	// model to recover (e.g., rewriter error with actionable
	// guidance) by feeding the refusal text back as a synthetic
	// tool_result. Distinct from Continue which builds a full
	// synthetic assistant turn — this is just the tool_result text.
	ContinueWithToolResultText string
	// EmittedAuditExternally signals that this evaluator already emitted
	// an audit row via a side channel (the legacy trigger-miss authorizer
	// closure pattern emits via its own emit callback rather than
	// returning AuditFields the orchestrator's emitter will translate).
	// Set true to suppress the orchestrator's downstream audit emission
	// for this verdict. Typed replacement for the AuditFields key
	// inspection the pipelineeval factory used to perform.
	EmittedAuditExternally bool
	// HeldKind classifies the verdict for postproc's coalescing pass.
	// Evaluators that want to influence coalescing set it explicitly;
	// when empty, classifyVerdict falls back to substring matching on
	// Reason. Phase 6 deletes the fallback.
	HeldKind HeldKindHint
	// Facts carries typed observations an evaluator emitted about this
	// tool_use. Audit emission and downstream consumers branch via
	// type switch on Facts; the AuditFields map[string]any path is
	// kept only as a transitional carrier for evaluators not yet
	// migrated. Facts are populated for EVERY evaluator that runs,
	// including those returning Skip — observation is a separate
	// channel from verdict claiming.
	Facts []EvaluationFact
	// SubstituteText, when non-empty, replaces the tool_use block with
	// a plain-text assistant block in the rewritten response. Used by
	// approval-prompt rendering, inline-task interception, etc.
	// Translates to conversation.ToolUseVerdict.SubstituteWith via the
	// bridge.
	SubstituteText string
	// RewriteInput, when non-nil, replaces the tool_use's input field
	// in-place. Used by inspector/control rewrites where the URL or
	// args change but the tool_use shape is preserved.
	RewriteInput []byte
	// ContinueWithToolResult, when non-empty, feeds back a synthetic
	// tool_result to the upstream LLM so it continues with its next
	// tool_use. Replaces the older ContinueWithToolResultText.
	ContinueWithToolResult string
	// PrependAssistantNotice, when non-empty, prepends a user-facing
	// notice to the assistant turn returned after a successful
	// ContinueWithToolResult round-trip ("a task was auto-approved on
	// your behalf"). Ignored when ContinueWithToolResult is empty.
	PrependAssistantNotice string
	// CreatedTaskID is set by the conversation auto-approval gate to
	// the ID of the inline task it created before returning the
	// verdict. Carried so downstream audit rows can link to the same
	// task_id without parsing augmentation text.
	CreatedTaskID string
}

// HeldKindHint classifies a verdict for the coalescing pass without
// the substring-on-Reason heuristic the legacy classifier used. Values
// match the string form of llmproxy.HeldToolUseKind so the
// conversation-side bridge can propagate one-for-one. Empty value
// means "no hint; let the legacy classifier infer."
type HeldKindHint string

const (
	HeldKindHintApproval HeldKindHint = "approval"
	HeldKindHintAllow    HeldKindHint = "allow"
	HeldKindHintRewrite  HeldKindHint = "rewrite"
	HeldKindHintDeny     HeldKindHint = "deny"
)

// SyntheticResponse is returned to the client when a RequestPolicy
// short-circuits the forward step (e.g., inline-task-approval resolution
// returns a synthesized assistant turn directly).
type SyntheticResponse struct {
	// Body bytes in the *upstream* provider format. The post-phase
	// runs against this body so downstream policies (notice injection,
	// audit) still apply uniformly.
	Body []byte
	// StatusCode reported to the client. Typically 200.
	StatusCode int
	// Headers applied to the client response (Content-Type at minimum).
	Headers map[string]string
	// Streaming indicates whether the body should be streamed back via
	// SSE framing (true) or returned as a single buffered response.
	Streaming bool
}

// ContinueSignal is returned by a ToolUseEvaluator when the tool_use is
// being served locally and the pipeline should re-enter with a
// synthetic continuation turn.
type ContinueSignal struct {
	// SyntheticAssistantBlocks is the assistant turn the pipeline
	// appends in place of the upstream response.
	SyntheticAssistantBlocks []json.RawMessage
	// SyntheticToolResults is the user-role tool_result message that
	// closes the local turn.
	SyntheticToolResults []json.RawMessage
	// PrependNotice is an optional human-visible notice the assistant
	// turn should carry (e.g., "task auto-approved").
	PrependNotice string
}

// ByteSpan is a [start, end) byte range used for span-based redaction.
// Used by secret_detection to redact spans in the original body without
// re-parsing.
type ByteSpan struct {
	Start int
	End   int
}

// SyntheticContinuation is the typed shape policies can build via
// RequestMutator.AppendContinuationTurn. Used by continuation re-entry
// to construct the next-turn body.
type SyntheticContinuation struct {
	AssistantBlocks []json.RawMessage
	ToolResults     []json.RawMessage
}
