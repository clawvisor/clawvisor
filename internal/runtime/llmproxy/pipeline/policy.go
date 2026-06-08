package pipeline

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// Outcome aliases conversation.Outcome so pipeline code can keep using
// the local name while the canonical definition lives in conversation.
type Outcome = conversation.Outcome

const (
	OutcomeAllow        = conversation.OutcomeAllow
	OutcomeDeny         = conversation.OutcomeDeny
	OutcomeHold         = conversation.OutcomeHold
	OutcomeRewrite      = conversation.OutcomeRewrite
	OutcomeShortCircuit = conversation.OutcomeShortCircuit
	OutcomeSkip         = conversation.OutcomeSkip
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
// The policies package composes the concrete inspector, authorization,
// task-scope, intent-verification, and rewrite stages into this shape.
type ToolUseEvaluator interface {
	Name() string
	Evaluate(ctx context.Context, res ReadOnlyResponse, tu conversation.ToolUse, mut ToolUseMutator) (ToolUseVerdict, error)
}

// ReadOnlyRequest exposes the parsed, lossy view of the inbound request.
// Mutations never go through here — they go through RequestMutator.
//
// First-turn detection and conversation-ID minting happen upstream of
// policies, so policies see stable values.
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
	// ValidateReplacementBody verifies bytes passed to
	// RequestMutator.ReplaceBody for this request's provider/body shape.
	// Adapters that do not have a provider parser may return nil.
	ValidateReplacementBody([]byte) error
}

// ReadOnlyResponse exposes the response under inspection.
//
// For buffered responses, ToolUses returns the full set immediately.
// Streaming callers may collect tool uses incrementally as events arrive,
// but response-level evaluation runs once the complete sibling set is
// known so coalescing and audit decisions see the full turn.
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
	AuditParams map[string]any
	// ShortCircuit is set when Outcome == ShortCircuit. The pipeline
	// skips remaining pre policies AND the forward step; the synthetic
	// body enters the post-phase as if it were an upstream response.
	ShortCircuit *SyntheticResponse
}

// ResponseVerdict is the result of a ResponsePolicy.Postprocess call.
type ResponseVerdict struct {
	Outcome     Outcome
	Reason      string
	AuditParams map[string]any
}

// ToolUseVerdict aliases conversation.ToolUseVerdict — the unified
// verdict shape both pipeline evaluators and response rewriters
// consume.
type ToolUseVerdict = conversation.ToolUseVerdict

// HeldKindHint aliases conversation.HeldKindHint.
type HeldKindHint = conversation.HeldKindHint

const (
	HeldKindHintApproval = conversation.HeldKindHintApproval
	HeldKindHintAllow    = conversation.HeldKindHintAllow
	HeldKindHintRewrite  = conversation.HeldKindHintRewrite
	HeldKindHintDeny     = conversation.HeldKindHintDeny
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

// ContinueSignal aliases conversation.ContinueSignal.
type ContinueSignal = conversation.ContinueSignal

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
