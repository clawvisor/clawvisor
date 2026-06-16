package llmproxy

// Types + the factory hook the postproc package consumes when it
// orchestrates a response.
//
// Postprocess + PostprocessStream live in
// internal/runtime/llmproxy/postproc. The policies chain consumes
// smaller helper packages for body transforms, control-plane parsing,
// approval prompt text, placeholder boundaries, history stripping,
// intent verification, script-session recognition, and task checkout
// state; root-level wrappers remain here for existing callers.

import (
	"context"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/intentverify"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/scriptjudge"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// IntentVerifier matches the intent.Verifier contract. The lite-proxy
// declares its own narrow interface to avoid pulling the LLM provider
// dependency into this package.
type IntentVerifier = intentverify.Verifier

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

type IntentVerifyRequest = intentverify.Request
type IntentVerdict = intentverify.Verdict

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
type ToolUseEvaluatorFactory func(req *http.Request, cfg PostprocessConfig, provider conversation.Provider, toolUses []conversation.ToolUse, emit func(conversation.AuditEvent)) conversation.ToolUseEvaluator

// AgentContext groups identity carriers — user, agent, and agent
// display name — that flow into most stages.
type AgentContext struct {
	AgentUserID string
	AgentID     string
	AgentName   string
}

// AuditContext groups the audit emitter + request correlation + trace
// sink so the audit-emission path has a single typed dependency.
type AuditContext struct {
	Audit          *AuditEmitter
	RequestID      string
	ConversationID string
	Trace          *TraceLogger
}

// AuthorizationContext groups the decision-engine inputs +
// catalog/scope checkers that authorize tool_uses.
type AuthorizationContext struct {
	Posture         runtimedecision.EvaluationPosture
	CandidateTasks  []*store.Task
	ToolRules       []*store.RuntimePolicyRule
	EgressRules     []*store.RuntimePolicyRule
	PreferredTaskID string
	TaskScope       TaskScopeChecker
	IntentVerifier  IntentVerifier
	// ScopeDrifts holds per-block drift records used by the scope-drift
	// continuation menu. Nil disables the menu and falls back to the
	// pre-existing inline approval prompt.
	ScopeDrifts ScopeDriftRegistry
	Catalog     interface {
		Resolve(host, method, path string) (ResolvedAction, bool)
	}
}

// ApprovalContext groups the inline-approval flow's dependencies:
// pending cache, risk assessor, the human-context turns the assessor
// reads, the inline-task-creator, the auto-approve threshold + default
// expiry, the checkout registry, and the inbound tool list the
// renderer reads to pick the substitution shape.
type ApprovalContext struct {
	PendingApprovals                 PendingApprovalCache
	TaskRiskAssessor                 TaskRiskAssessor
	RecentUserTurns                  []string
	InlineTaskCreator                InlineTaskCreator
	ConversationAutoApproveThreshold string
	Checkouts                        TaskCheckoutStore
	DefaultTaskExpirySeconds         int
	// AvailableTools is the inbound request's declared tool list
	// (e.g. Anthropic tools[].name). Consumed by the inline-approval
	// intercept to decide whether to substitute the held tool_use
	// with an AskUserQuestion picker (when that harness tool is
	// declared) or a plain text prompt.
	//
	// Technically per-request request metadata rather than an
	// approval-flow input, but living on ApprovalContext keeps the
	// existing buildControlResolver param-passing pattern intact:
	// the intercept is built from sub-contexts, and a top-level
	// PostprocessConfig field was demonstrably easy to forget to
	// plumb through.
	AvailableTools []string
}

// RewriteContext groups the credentialed-rewrite path's dependencies:
// inspector + rewrite opts + caller-nonce cache + placeholder store.
type RewriteContext struct {
	Inspector    *inspector.Inspector
	RewriteOpts  inspector.RewriteOpts
	CallerNonces CallerNonceCache
	Store        store.Store
}

// ScriptSessionContext groups the script-session evaluator's
// dependencies. Carries the use-time LLM judge that re-classifies
// tool_uses the deterministic recognizer flags as URL-unrecognized
// (variable-ized URL/header, Write+Bash staging, language wrappers).
// Nil judge disables the LLM path — the chain falls through to the
// inspector's generic refusal.
//
// Lives in its own sub-context because the judge is a recognition
// dependency, not a rewrite dependency. The resolver base URL still
// comes from RewriteContext.RewriteOpts (it's a routing concern).
type ScriptSessionContext struct {
	Judge scriptjudge.Judge
}

// RoutingContext groups the response-routing dependencies: the
// control-plane synthetic host, the first-turn routing notice, and the
// registry of response rewriters.
type RoutingContext struct {
	ControlBaseURL   string
	FirstTurnNotice  string
	ResponseRegistry *conversation.ResponseRegistry
}

// PostprocessConfig wires the inspector + rewriter into the LLM
// endpoint handler's response path. The handler reads the upstream
// response body and calls postproc.Postprocess; the result is what
// the harness sees.
//
// Embedded sub-contexts group dependencies by stage while preserving
// field-access promotion (cfg.AgentID, cfg.RequestID, etc.) for
// existing internal call sites.
type PostprocessConfig struct {
	ToolUseEvaluatorFactory ToolUseEvaluatorFactory
	AgentContext
	AuditContext
	AuthorizationContext
	ApprovalContext
	RewriteContext
	ScriptSessionContext
	RoutingContext
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
