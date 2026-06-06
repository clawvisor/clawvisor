package llmproxy

// Types + the factory hook the postproc package consumes when it
// orchestrates a response.
//
// Postprocess + PostprocessStream live in
// internal/runtime/llmproxy/postproc; the helpers the policies chain
// consumes (EvaluateTriggerMissAuthorization,
// EvaluateCredentialedAuthorization, MaybeInterceptInlineTaskDefinition,
// CredentialedRewriteRecoveryReason, ScriptSessionToolUse,
// ControlToolUseMentionsEndpoint) live alongside this file in
// postprocess.go + trigger_miss_authorization.go +
// credentialed_authorization.go + script_session_helpers.go.

import (
	"context"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// IntentVerifier matches the intent.Verifier contract. The lite-proxy
// declares its own narrow interface to avoid pulling the LLM provider
// dependency into this package.
type IntentVerifier interface {
	Verify(ctx context.Context, req IntentVerifyRequest) (*IntentVerdict, error)
}

// TaskRiskAssessor scores a candidate task envelope at creation time so
// the inline-approval prompt can surface a real, LLM-judged risk read
// instead of the deterministic fallback. Narrow interface so this
// package doesn't pull in the taskrisk LLM client dependency.
type TaskRiskAssessor interface {
	AssessEnvelope(ctx context.Context, req TaskRiskAssessRequest) *TaskRiskAssessment
}

// TaskRiskAssessRequest is the per-task input to TaskRiskAssessor. It
// mirrors taskrisk.AssessRequest's v2-envelope shape; the handler
// adapter is responsible for translating between the two so this
// package can stay independent of the taskrisk package.
type TaskRiskAssessRequest struct {
	Purpose                string
	AgentName              string
	UserID                 string
	ExpectedTools          []runtimetasks.ExpectedTool
	ExpectedEgress         []runtimetasks.ExpectedEgress
	RequiredCredentials    []runtimetasks.RequiredCredential
	IntentVerificationMode string
	ExpectedUse            string
	// RecentUserTurns carries the user's recent human-authored chat
	// turns (chronological, most recent last) so the assessor can
	// evaluate whether the conversation context authorizes this task.
	// When non-empty, the assessor emits an IntentMatch verdict on the
	// returned TaskRiskAssessment; empty means the assessor falls back
	// to scope-only judgment. Treated as UNTRUSTED data by the
	// assessor's prompt — never used as instruction.
	RecentUserTurns []string
}

// TaskRiskAssessment mirrors taskrisk.RiskAssessment but lives in this
// package to keep the dependency narrow.
type TaskRiskAssessment struct {
	RiskLevel              string
	Explanation            string
	Factors                []string
	IntentMatch            string
	IntentMatchExplanation string
	Conflicts              []TaskRiskConflict
}

// TaskRiskConflict is the lite-proxy projection of taskrisk.ConflictDetail.
type TaskRiskConflict struct {
	Field       string
	Description string
	Severity    string
}

// IntentVerifyRequest is the per-tool-use input to the verifier.
type IntentVerifyRequest struct {
	TaskPurpose string
	ExpectedUse string
	Service     string
	Action      string
	Params      map[string]any
	Reason      string
	TaskID      string
	Lenient     bool
}

// IntentVerdict mirrors intent.VerificationVerdict.
type IntentVerdict struct {
	Allow       bool
	Explanation string
}

// BufferedAudit aliases conversation.AuditEvent — Phase 9 unified the
// typed observation record (Outcome enum + Facts) and the audit wire
// shape (Decision string + OutcomeName string + InspectorVerdict +
// TaskID) into a single struct.
type BufferedAudit = conversation.AuditEvent

// ToolUseEvaluatorFactory, when set on PostprocessConfig, replaces the
// orchestrator's default tool_use evaluator with a handler-supplied
// implementation (typically the policies-chain-based pipeline
// evaluator). The factory receives the request, full config,
// provider, the tool_use list (pre-extracted for the buffered path
// so the pipeline can run response-level; empty for streaming where
// tool_uses arrive incrementally), and an emit callback that the
// factory uses to append audit rows to the internal sink.
//
// When toolUses is non-empty, the factory pre-runs pipeline
// evaluation ONCE on the full sibling set, emitting audits + holds
// up front; the returned per-tool eval is a verdict lookup. When
// empty, the factory falls back to lazy per-call pipeline runs
// (used by the streaming path that doesn't have the full list
// available before the rewriter sees the response).
type ToolUseEvaluatorFactory func(req *http.Request, cfg PostprocessConfig, provider conversation.Provider, toolUses []conversation.ToolUse, emit func(BufferedAudit)) conversation.ToolUseEvaluator

// PostprocessConfig wires the inspector + rewriter into the LLM
// endpoint handler's response path. The handler reads the upstream
// response body and calls postproc.Postprocess; the result is what
// the harness sees.
type PostprocessConfig struct {
	ToolUseEvaluatorFactory ToolUseEvaluatorFactory

	// Inspector decides whether each tool_use should be rewritten or
	// passed through. Required.
	Inspector *inspector.Inspector

	// RewriteOpts controls how the rewriter produces the redirected
	// tool_use input. Required when rewrite paths fire.
	RewriteOpts inspector.RewriteOpts

	// Store provides placeholder lookup for the boundary check.
	Store store.Store

	// AgentUserID + AgentID scope placeholder ownership to the calling
	// agent. Required for the boundary check.
	AgentUserID string
	AgentID     string

	// ConversationID is a stable per-conversation identifier extracted
	// from the incoming request body.
	ConversationID string

	// CallerNonces mints the short-lived single-use nonce that takes
	// the place of the agent's bearer token in the rewritten tool_use's
	// X-Clawvisor-Caller header.
	CallerNonces CallerNonceCache

	// Audit is the emitter for runtime.llm_proxy.* events.
	Audit *AuditEmitter

	// RequestID is the audit RequestID for tool_use rows so they group
	// with the parent endpoint call.
	RequestID string

	// ResponseRegistry is the conversation rewriter registry.
	ResponseRegistry *conversation.ResponseRegistry

	// Catalog reverse-resolves (host, method, path) → (service, action).
	Catalog interface {
		Resolve(host, method, path string) (ResolvedAction, bool)
	}

	// TaskScope authorizes the resolved (service, action) against the
	// agent's active tasks.
	TaskScope TaskScopeChecker

	// IntentVerifier runs the LLM intent check against the matched
	// TaskAction's expected_use.
	IntentVerifier IntentVerifier

	// Decision-engine inputs.
	Posture         runtimedecision.EvaluationPosture
	CandidateTasks  []*store.Task
	ToolRules       []*store.RuntimePolicyRule
	EgressRules     []*store.RuntimePolicyRule
	PreferredTaskID string

	PendingApprovals PendingApprovalCache

	// TaskRiskAssessor scores a task envelope via LLM at inline-approval
	// time so the approval prompt carries an evaluated risk read.
	TaskRiskAssessor TaskRiskAssessor

	// AgentName is the agent's display name, surfaced to the assessor.
	AgentName string

	// RecentUserTurns is forwarded to the task-risk assessor's
	// intent-match read.
	RecentUserTurns []string

	// ControlBaseURL is the synthetic control-plane host the proxy
	// rewrites control tool_uses against.
	ControlBaseURL string

	// Trace is the optional JSONL telemetry sink.
	Trace *TraceLogger

	// InlineTaskCreator handles the inline-task-approval intercept.
	InlineTaskCreator InlineTaskCreator

	// ConversationAutoApproveThreshold controls when the conversation
	// auto-approve gate fires.
	ConversationAutoApproveThreshold string

	// Checkouts is the task-checkout registry.
	Checkouts TaskCheckoutStore

	// DefaultTaskExpirySeconds is the default TTL for inline-created
	// tasks when the model omits expires_in_seconds.
	DefaultTaskExpirySeconds int

	// FirstTurnNotice is the routing notice the streaming flow prepends
	// to the first SSE assistant turn (text-only).
	FirstTurnNotice string
}

// PostprocessResult is what postproc.Postprocess +
// postproc.PostprocessStream return to the handler.
type PostprocessResult struct {
	Body          []byte
	ContentType   string
	Rewritten     bool
	Decisions     []conversation.ToolUseDecisionRecord
	SkippedReason string

	// ContinuationToolResults carries the synthetic tool_result
	// payloads the proxy wants to feed back upstream as a continuation
	// turn.
	ContinuationToolResults []conversation.ContinuationToolResult

	// AssistantTurn is the upstream's assistant turn the streaming
	// path captured.
	AssistantTurn *conversation.Turn

	// StreamingProvider names the provider whose streaming shape the
	// rewriter consumed.
	StreamingProvider conversation.Provider

	// StreamingResult carries the streaming rewrite metadata (next
	// content-index, stream IDs, etc.).
	StreamingResult conversation.StreamingRewriteResult
}

