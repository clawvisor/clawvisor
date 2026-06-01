package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/runtime/toolnames"
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
// package to keep the dependency narrow. The renderer only consumes
// RiskLevel + Explanation; the rest is passed through for parity with
// the dashboard surface.
type TaskRiskAssessment struct {
	RiskLevel   string
	Explanation string
	Factors     []string
	// IntentMatch reports whether the user's recent chat turns
	// unambiguously authorize the requested scope. Set only when
	// RecentUserTurns was supplied in the request and the assessor
	// returned a verdict; "unknown" otherwise. Values:
	// "yes" | "partial" | "no" | "unknown".
	IntentMatch string
	// IntentMatchExplanation is a 1-sentence rationale for IntentMatch.
	IntentMatchExplanation string
	// Conflicts mirrors taskrisk.ConflictDetail entries. The
	// auto-approve gate refuses to fire when this slice is non-empty,
	// regardless of intent_match or risk_level — a conflict means the
	// task is internally inconsistent and the human should see it.
	Conflicts []TaskRiskConflict
}

// TaskRiskConflict is the lite-proxy projection of taskrisk.ConflictDetail.
// Kept narrow to avoid pulling the taskrisk dependency into this package.
type TaskRiskConflict struct {
	Field       string
	Description string
	Severity    string
}

// IntentVerifyRequest is the per-tool-use input to the verifier. Mirrors
// the gateway's intent.VerifyRequest but stripped down to fields the
// lite-proxy can populate from the inspector verdict + matched task.
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

// IntentVerdict mirrors intent.VerificationVerdict (Allow + Explanation
// are the fields lite-proxy actually consumes).
type IntentVerdict struct {
	Allow       bool
	Explanation string
}

// PostprocessConfig wires the inspector + rewriter into the LLM endpoint
// handler's response path. The handler reads the upstream response body
// and calls Postprocess; the result is what the harness sees.
type PostprocessConfig struct {
	// Inspector decides whether each tool_use should be rewritten or
	// passed through. Required.
	Inspector *inspector.Inspector

	// RewriteOpts controls how the rewriter produces the redirected
	// tool_use input. Required when rewrite paths fire.
	RewriteOpts inspector.RewriteOpts

	// Store provides placeholder lookup for the boundary check. The
	// validator's claimed Host is rebound to the placeholder's bound
	// service host allowlist; mismatch fails closed. Required when
	// rewrites are enabled.
	Store store.Store

	// AgentUserID + AgentID scope placeholder ownership to the calling
	// agent. Required for the boundary check.
	AgentUserID string
	AgentID     string

	// ConversationID is a stable per-conversation identifier extracted from
	// the incoming request body (see conversation.ConversationID). Used to
	// scope pending-approval holds and task checkout focus so multiple
	// conversations sharing a Clawvisor token don't clobber each other.
	// Empty falls back to the pre-conversation-scoping behavior — empty
	// IDs collide rather than partition, matching old clients.
	ConversationID string

	// CallerNonces mints the short-lived single-use nonce that takes
	// the place of the agent's bearer token in the rewritten tool_use's
	// X-Clawvisor-Caller header. The nonce is bound to (agent, host,
	// method, path); the resolver-side middleware consumes it on the
	// matching call. When non-nil, the rewriter receives a freshly
	// minted nonce per tool_use; the agent's raw token never enters
	// the model's conversation context. When nil, credentialed rewrites
	// fail closed with a configuration error.
	CallerNonces CallerNonceCache

	// Audit is the emitter for runtime.llm_proxy.* events. nil disables
	// audit logging from the postprocess path. The handler keeps audit
	// for the endpoint-call shape; postprocess adds per-tool-use rows.
	Audit *AuditEmitter

	// RequestID is the audit RequestID for tool_use rows so they group
	// with the parent endpoint call.
	RequestID string

	// ResponseRegistry is the conversation rewriter registry. Defaults
	// to conversation.DefaultResponseRegistry() when nil.
	ResponseRegistry *conversation.ResponseRegistry

	// Catalog reverse-resolves (host, method, path) → (service, action)
	// so the task-scope checker can decide whether an active task covers
	// this call. Optional: when nil, task-scope is skipped (v0 fail-open
	// for backwards compatibility on deployments without it wired).
	Catalog interface {
		Resolve(host, method, path string) (ResolvedAction, bool)
	}

	// TaskScope authorizes the resolved (service, action) against the
	// agent's active tasks. Optional: when nil, task-scope is skipped.
	// Skipping is audited so dashboards can show the gap.
	TaskScope TaskScopeChecker

	// IntentVerifier runs the LLM intent check against the matched
	// TaskAction's expected_use whenever the matched action's
	// Verification mode is "strict" (default) or "lenient". Optional:
	// when nil, intent verification is skipped.
	IntentVerifier IntentVerifier

	// Shared decision evaluator inputs. When any of these are set,
	// Postprocess authorizes through pkg/runtime/decision after inspector
	// boundary validation. When all are nil, it falls back to the legacy
	// Catalog/TaskScope flow for compatibility with older tests/configs.
	Posture         runtimedecision.EvaluationPosture
	CandidateTasks  []*store.Task
	ToolRules       []*store.RuntimePolicyRule
	EgressRules     []*store.RuntimePolicyRule
	PreferredTaskID string

	PendingApprovals PendingApprovalCache

	// TaskRiskAssessor scores a task envelope via LLM at inline-approval
	// time so the approval prompt carries an evaluated risk read.
	// Optional: when nil, the intercept falls back to the deterministic
	// envelope-shape policy alone.
	TaskRiskAssessor TaskRiskAssessor

	// AgentName is the agent's display name, surfaced to the assessor so
	// its prompt context matches the dashboard task-creation surface.
	// Optional.
	AgentName string

	// RecentUserTurns is the user's recent human-authored chat turns,
	// extracted from the inbound LLM request by the handler. Passed to
	// the risk assessor so it can emit an intent_match verdict, and
	// consulted by the auto-approve gate to decide whether the
	// conversation context covers the task being created. Empty when
	// the handler couldn't extract any genuine human turns from the
	// inbound body. Optional.
	RecentUserTurns []string

	// ConversationAutoApproveThreshold is the user's per-account cap
	// for conversation-based auto-approval ("off" | "low" | "medium" |
	// "high" | "critical"; "off" by default). When the assessor's
	// risk_level is at-or-below this threshold AND intent_match=="yes"
	// AND no conflicts are present, the inline-task intercept skips
	// the human approval prompt and pre-approves the task. The gate's
	// comparison logic accepts any documented level; the
	// product/UI/API cap is enforced at write time, not here.
	ConversationAutoApproveThreshold string

	// InlineTaskCreator is the handler-supplied bridge that creates an
	// inline-approved task on the user's behalf. Required for the
	// conversation-based auto-approval path; when nil, the gate cannot
	// fire and the intercept falls back to the human approval prompt.
	// Same interface used by the post-yes release path, so the wire
	// shape stays uniform.
	InlineTaskCreator InlineTaskCreator

	// Checkouts records the active task per (user, agent). When the
	// auto-approve gate fires, the newly created task is set as the
	// active checkout — matching the manual "yes" flow's behavior so
	// subsequent tool calls land under the new task. Optional; nil
	// disables checkout side-effects but doesn't block auto-approval.
	Checkouts TaskCheckoutStore

	// ControlBaseURL is the daemon URL used for synthetic Clawvisor control
	// endpoint rewrites. Empty disables the control-plane rewrite path.
	ControlBaseURL string

	// DefaultTaskExpirySeconds is the daemon's resolved
	// task.default_expiry_seconds, surfaced into the inline task
	// approval prompt so the displayed Duration tracks the operator's
	// configured default when the agent omits expires_in_seconds.
	// Optional: when 0 or unset, the renderer falls back to its
	// historical 30-minute constant — the actual scope-binding
	// fallback used by tasks_inline.go is unaffected either way.
	DefaultTaskExpirySeconds int

	// Trace, when non-nil, receives one JSON-line event per inspector
	// decision point for this request. Disabled by default; enabled
	// via cfg.ProxyLite.TraceLogPath. Calls on a nil *TraceLogger are
	// no-ops, so production code doesn't branch.
	Trace *TraceLogger

	// FirstTurnNotice, when non-empty, is the assistant-text notice the
	// streaming postprocess path prepends to the response so the user
	// sees a one-liner ("[Clawvisor] Routing this conversation…")
	// before the model's first content on a fresh conversation. Only
	// consulted by PostprocessStream; the buffered Postprocess path
	// keeps its existing inline prepend at the handler level. Empty
	// disables injection.
	FirstTurnNotice string
}

// PostprocessResult reports what happened during postprocess. The handler
// uses it to log audit events and surface decisions.
type PostprocessResult struct {
	// Body is the post-processed response body to return to the harness.
	// Identical to the input body when no rewrites applied.
	Body []byte

	// ContentType is the response Content-Type to return.
	ContentType string

	// Rewritten reports whether any tool_use was mutated.
	Rewritten bool

	// Decisions is the per-tool-use audit trail produced by the inspector.
	Decisions []conversation.ToolUseDecisionRecord

	// Skipped reports paths where rewrite logic was bypassed (e.g.
	// streaming SSE in v0). Empty when the response was processed.
	SkippedReason string

	// ContinuationToolResults is non-empty if a streaming turn triggered a continuation
	ContinuationToolResults []conversation.ContinuationToolResult

	// AssistantTurn is the accumulated turn details for continuation building
	AssistantTurn *conversation.Turn

	// Streaming fallback state is populated when a streamed turn requested
	// continuation. It is rendered before PostprocessStream returns so the
	// caller receives one complete provider-shaped stream.
	StreamingProvider conversation.Provider
	StreamingResult   conversation.StreamingRewriteResult
}

// Postprocess inspects an upstream response body and applies tool_use
// rewrites where the inspector + boundary check allow. It honors the
// existing block-or-pass evaluator semantics and adds the rewrite path.
//
// Both JSON and SSE Anthropic responses are handled; the SSE path
// whole-buffers the upstream stream, parses it, and re-emits a
// synthesized SSE turn with rewritten tool_use input bytes substituted
// in. Streaming-while-rewriting (true block-by-block emit) is a future
// optimization — the response shape the harness sees is identical.
//
// Returns the response body the handler should write to the harness.
func Postprocess(req *http.Request, body []byte, contentType string, cfg PostprocessConfig) PostprocessResult {
	if cfg.Inspector == nil {
		return PostprocessResult{Body: body, ContentType: contentType, SkippedReason: "no inspector configured"}
	}

	registry := cfg.ResponseRegistry
	if registry == nil {
		registry = conversation.DefaultResponseRegistry()
	}

	// MatchesResponse on the existing rewriters checks the request's host;
	// for the lite-proxy endpoint the host is `clawvisor.example`, not
	// `api.anthropic.com`. Use the parser registry instead — it's
	// route-keyed via ParserForRoute (added for lite-proxy).
	rewriter := matchByRoute(req, registry)
	if rewriter == nil {
		return PostprocessResult{Body: body, ContentType: contentType, SkippedReason: "no rewriter for route"}
	}

	auditAgent := auditAgentForCfg(cfg)

	// Coalescence capture state. Pass 1 runs with a buffering wrapper
	// over both PendingApprovals and the audit emission so we can:
	//   * detect when multiple tool_uses in one turn need approval
	//   * detect the inline-task path (Stage != StageTool) to skip
	//     coalescence for it
	//   * decide a final shape (legacy: replay buffers; coalesce:
	//     discard buffers and write one coalesced hold + per-tool
	//     coalesced-pending audit rows)
	// Buffering — rather than passthrough-and-then-cleanup — closes
	// two hazards. (a) Misleading audit: in passthrough mode, pass 1
	// would emit "allow"/"rewrite" rows for siblings whose calls then
	// get replaced by a coalesced approval; dashboards would believe
	// they executed. (b) Spurious eviction: bounded caches near
	// capacity could displace an unrelated older approval while N
	// per-tool holds are temporarily resident. Buffering keeps the
	// underlying state untouched until the final shape is decided.
	originalPendingApprovals := cfg.PendingApprovals
	holdSink := &capturedHoldSink{}
	if originalPendingApprovals != nil {
		cfg.PendingApprovals = newHoldCapturingApprovalCache(originalPendingApprovals, holdSink)
	}
	auditSink := &capturedAuditSink{}
	var captures []evalCapture

	innerEval := newToolUseEvaluator(req, cfg, rewriter.Name(), auditSink)

	// Outer eval wraps innerEval and records the kind + decision
	// context for the coalesce post-pass. Two side channels feed the
	// capture:
	//   * holdSink.holds — populated by the (buffered) PendingApprovals.Hold
	//     wrapper when innerEval creates a per-tool hold. Carries the
	//     hold ID, stage, and the full inspector/fingerprint/reason
	//     bundle the eval body assembled before calling Hold.
	//   * auditSink.entries — populated by the (buffered) audit closure
	//     on every audit() call inside innerEval. The last entry for
	//     this call carries the inspector verdict and the final reason
	//     even when no hold was created (auto-allow, auto-rewrite,
	//     hard deny). Without this, coalesced sibling release audit
	//     rows would have empty target_host/method/path because no
	//     hold captured them.
	// Fingerprint is captured only via the hold sink, because the
	// release path's EquivalentFingerprint check only fires for
	// HeldKindApproval entries — non-approval siblings either pass
	// through, deny outright, or fail-closed with "re-prompt needed."
	eval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		holdsBefore, auditsBefore := 0, 0
		if holdSink != nil {
			holdsBefore = len(holdSink.holds)
		}
		if auditSink != nil {
			auditsBefore = len(auditSink.entries)
		}
		v := innerEval(tu)
		c := evalCapture{Use: tu, Kind: classifyVerdict(v)}
		if holdSink != nil && len(holdSink.holds) > holdsBefore {
			h := holdSink.holds[len(holdSink.holds)-1]
			c.HoldID = h.Pending.ID
			c.Stage = h.Pending.Stage
			c.Inspector = h.Pending.Inspector
			c.Fingerprint = h.Pending.Fingerprint
			c.Reason = h.Pending.Reason
		} else if auditSink != nil && len(auditSink.entries) > auditsBefore {
			last := auditSink.entries[len(auditSink.entries)-1]
			c.Inspector = last.Verdict
			c.Reason = last.Reason
		}
		// TaskID is sourced from the audit sink (not the hold sink)
		// because the hold sink doesn't carry it — the matched task
		// is recorded by the audit closure right after EvaluateAuthorization
		// or TaskScope.Check resolves it. Both the held and the
		// non-held branches above leave any matched task on the most
		// recent audit entry, so reading it here picks it up
		// regardless of whether a hold was created.
		if auditSink != nil && len(auditSink.entries) > auditsBefore {
			c.TaskID = auditSink.entries[len(auditSink.entries)-1].TaskID
		}
		captures = append(captures, c)
		return v
	}
	failClosed := func(reason string) PostprocessResult {
		rollbackBufferedPendingTasks(req.Context(), cfg, holdSink)
		return PostprocessResult{
			Body:          nil,
			ContentType:   contentType,
			SkippedReason: reason,
		}
	}

	result, err := rewriter.Rewrite(body, contentType, eval)
	if err != nil {
		// Fail closed: the rewriter failed mid-body so we don't know
		// whether a credentialed placeholder survived into the response.
		// Returning the original body would pass it (or worse, the
		// literal placeholder) to the harness. Drop the body and surface
		// a non-empty SkippedReason; the handler checks SkippedReason to
		// emit a 502 instead of writing the upstream body unchanged.
		return failClosed("rewriter error: " + err.Error())
	}

	// Coalesce decision. When the turn carries multiple tool_uses and
	// at least one needs approval (and the inline-task flow is not in
	// play), replace the buffered per-tool holds with one coalesced
	// hold covering the whole turn and rewrite the buffered audit so
	// it reports the calls as "coalesced approval pending" rather
	// than as if they had executed. A single user yes/no then
	// releases (or denies) all sibling calls together.
	if originalPendingApprovals != nil && shouldCoalesceTurn(captures) {
		coalesced := coalesceFromCaptures(captures)
		coalesced.UserID = cfg.AgentUserID
		coalesced.AgentID = cfg.AgentID
		coalesced.Provider = rewriter.Name()
		coalesced.ConversationID = cfg.ConversationID
		held, holdErr := originalPendingApprovals.Hold(req.Context(), coalesced)
		if holdErr == nil {
			// Coalesced hold committed. The buffered per-tool holds
			// were never inserted into the underlying cache, so
			// there's nothing to drop here — that closes the
			// bounded-cache eviction hazard for the buffered side.
			// The buffered audit rows are deliberately discarded:
			// they would have reported "allow"/"rewrite" for
			// siblings whose calls are now being held under the
			// coalesced approval, which is false in the audit
			// trail. We emit one "coalesced_approval_pending" row
			// per held tool_use instead so dashboards see what
			// actually happened.
			//
			// The coalesced Hold itself, though, CAN displace an
			// older inline-task hold from the underlying cache —
			// the bounded-cache hazard applies to this insert too.
			// Audit the eviction and terminate the displaced
			// inline-task row's DB anchor so the dashboard doesn't
			// strand it at pending_approval forever.
			if held.Evicted != nil {
				if cfg.Audit != nil && auditAgent != nil && len(captures) > 0 {
					// Attach the audit row to the first held
					// tool_use in the coalesced turn so dashboards
					// have a non-empty linkage; the reason string
					// names the evicted hold explicitly.
					first := captures[0]
					cfg.Audit.LogToolUseInspected(req.Context(), auditAgent, cfg.RequestID, first.Use, first.Inspector, "block", "approval_evicted", "superseded pending approval "+held.Evicted.ID, first.TaskID)
				}
				cleanupEvictedInlineTask(req.Context(), cfg, held.Evicted)
			}
			emitCoalescedPendingAuditRows(req.Context(), cfg, auditAgent, captures, held.Pending.ID)
			// Re-run the rewriter with a coalesced eval. Every
			// tool_use returns Allowed:false; the first carries the
			// combined prompt as SubstituteWith, the rest carry
			// empty SubstituteWith so the rewriter's join produces
			// one prompt (not N copies).
			coalescedPrompt := coalescedApprovalPrompt(held.Pending.AllHolds())
			firstReplaced := false
			coalescedEval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
				out := conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: approval required (coalesced turn) — " + held.Pending.Reason,
				}
				if !firstReplaced {
					out.SubstituteWith = coalescedPrompt
					firstReplaced = true
				}
				return out
			}
			coalescedResult, coalescedErr := rewriter.Rewrite(body, contentType, coalescedEval)
			if coalescedErr == nil {
				return PostprocessResult{
					Body:        coalescedResult.Body,
					ContentType: contentType,
					Rewritten:   true,
					Decisions:   coalescedResult.Decisions,
				}
			}
			// Coalesced re-run failed but the coalesced hold exists.
			// The first-pass body still references per-tool prompts
			// that no longer correspond to cache state (the
			// per-tool holds were never committed and the coalesced
			// hold is the only one now). Fall through to flush the
			// buffered audit (the rows describe what would have
			// happened) and return the first-pass body. Degraded
			// but recoverable: a user yes/no resolves the coalesced
			// hold via LIFO and the release synth emits every
			// approved call.
			flushBufferedAudit(req.Context(), cfg, auditAgent, auditSink)
			return PostprocessResult{
				Body:        result.Body,
				ContentType: contentType,
				Rewritten:   result.Rewritten,
				Decisions:   result.Decisions,
			}
		}
		// Hold-failure path: the coalesced hold could not be
		// committed. Fall through to legacy replay: write the
		// buffered per-tool holds to the underlying cache and flush
		// the buffered audit rows. The first-pass body already
		// describes those per-tool prompts; once they exist in the
		// cache the user's yes/no resolves them one by one (the
		// pre-coalesce path).
	}

	// Legacy replay: no coalescence happened (either shouldCoalesceTurn
	// said no, or the coalesced Hold failed). Commit the buffered
	// per-tool holds to the underlying cache and emit the buffered
	// audit rows as-is.
	if replayErr := replayBufferedHolds(req.Context(), cfg, originalPendingApprovals, holdSink, auditAgent, captures); replayErr != nil {
		// Fail closed: the first-pass body references approval
		// prompts whose holds couldn't be committed to the cache.
		// Returning the body would invite the user to type "yes" at
		// a prompt that resolves to nothing. Drop the body and
		// surface a non-empty SkippedReason so the handler emits
		// 502 — matches the pre-buffering eval path that returned
		// "Clawvisor: approval unavailable" inline when Hold failed.
		// Buffered audits are still flushed: they describe what
		// would have happened, and the SkippedReason adds the
		// approval-hold-storage row separately.
		flushBufferedAudit(req.Context(), cfg, auditAgent, auditSink)
		return failClosed("approval hold storage failed: " + replayErr.Error())
	}
	flushBufferedAudit(req.Context(), cfg, auditAgent, auditSink)

	return PostprocessResult{
		Body:        result.Body,
		ContentType: contentType,
		Rewritten:   result.Rewritten,
		Decisions:   result.Decisions,
	}
}

// rollbackBufferedPendingTasks expires any pending inline tasks created during
// the evaluation pass when the turn fails before its cache holds are safely
// committed. The task row is an operational orphan in this path, not a user
// denial, so use ExpireInlineTask to match eviction cleanup semantics.
func rollbackBufferedPendingTasks(ctx context.Context, cfg PostprocessConfig, sink *capturedHoldSink) {
	if sink == nil || len(sink.holds) == 0 {
		return
	}
	pendingCreator, ok := cfg.InlineTaskCreator.(InlineTaskPendingCreator)
	if !ok || pendingCreator == nil {
		return
	}
	// Inline-task interception currently creates at most one pending task per
	// turn; this bounded sequential rollback is fine for that invariant.
	// Parallelize if a future flow can buffer multiple pending inline tasks.
	for _, h := range sink.holds {
		if h.Pending.PendingTaskID == "" || h.Pending.UserID == "" {
			continue
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := pendingCreator.ExpireInlineTask(rollbackCtx, h.Pending.PendingTaskID, h.Pending.UserID)
		cancel()
		if err != nil && cfg.Trace != nil {
			cfg.Trace.Emit(map[string]any{
				"event":       "inline_task.rollback_expire_failed",
				"request_id":  cfg.RequestID,
				"user_id":     h.Pending.UserID,
				"agent_id":    h.Pending.AgentID,
				"approval_id": h.Pending.ID,
				"task_id":     h.Pending.PendingTaskID,
				"err":         err.Error(),
			})
		}
	}
}

func newToolUseEvaluator(req *http.Request, cfg PostprocessConfig, provider conversation.Provider, auditSink *capturedAuditSink) conversation.ToolUseEvaluator {
	return func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		var v inspector.Verdict
		// matchedTaskID is set by branches that resolve a task scope
		// (EvaluateAuthorization with a matched task, or
		// TaskScope.Check returning Allowed). audit() reads it into
		// the buffered row's TaskID so flushed audit rows carry the
		// matched task — that's what the dashboard's per-task activity
		// feed filters on (AuditEntry.task_id). Branches that never
		// matched a task (trigger-miss pass-through, hard denials
		// before any decision, control-tool inline-task creation where
		// the task is being created here) leave it empty.
		var matchedTaskID string
		audit := func(decision, outcome, reason string) {
			// Always buffer, even when no AuditEmitter is configured.
			// The outer eval wrapper reads the last entry for this
			// call to capture inspector metadata for coalesce
			// siblings (auto-allow / auto-rewrite calls that don't
			// create a hold). Without unconditional buffering, a
			// caller with cfg.Audit=nil would lose target_host/method/path
			// for those siblings in the coalesced hold's Additional
			// entries. The flush helpers already check cfg.Audit and
			// short-circuit, so this costs only a few struct copies
			// per call in audit-disabled deployments.
			auditSink.entries = append(auditSink.entries, bufferedAudit{
				ToolUse:  tu,
				Verdict:  v,
				Decision: decision,
				Outcome:  outcome,
				Reason:   reason,
				TaskID:   matchedTaskID,
			})
		}
		// trace emits one JSONL line per decision point when
		// cfg.Trace is configured. The kv slice is event-specific.
		trace := func(event string, kv ...any) {
			if cfg.Trace == nil {
				return
			}
			m := map[string]any{
				"event":       event,
				"request_id":  cfg.RequestID,
				"user_id":     cfg.AgentUserID,
				"agent_id":    cfg.AgentID,
				"tool_use_id": tu.ID,
				"tool_name":   tu.Name,
			}
			for i := 0; i+1 < len(kv); i += 2 {
				key, ok := kv[i].(string)
				if !ok {
					continue
				}
				m[key] = kv[i+1]
			}
			cfg.Trace.Emit(m)
		}
		trace(TraceEventToolUseEntry,
			"input_preview", truncateForTrace(string(tu.Input), traceInputPreviewLimit),
			"input_bytes", len(tu.Input),
			"trigger_hit", inspector.TriggerHits(inspector.ToolUse{ID: tu.ID, Name: tu.Name, Input: tu.Input}),
		)

		if call, ok := ParseControlToolUseWithBase(tu, cfg.ControlBaseURL); ok {
			v = call.Verdict
			// Inline task approval interception. When the user is
			// mid-flight on a "task" gesture (the original tool hold has
			// been transitioned to StageAwaitingTaskDefinition) and the
			// model now emits POST /api/control/tasks, we route the task body
			// through the inline approval path instead of letting it
			// proxy through to the dashboard. The model never sees the
			// real /api/control/tasks handler — its tool_use_result is
			// replaced with a rendered yes/no prompt; the user's next
			// "yes" creates the task pre-approved and the
			// follow-up turn auto-releases the original tool call.
			if inlineVerdict, inlineHandled := maybeInterceptInlineTaskDefinition(
				req, cfg, audit, trace, provider, tu, call,
			); inlineHandled {
				return inlineVerdict
			}
			// Mint a nonce bound to the rewritten control URL's
			// (host, method, path) — the rewritten curl carries it in
			// X-Clawvisor-Caller; the daemon's nonce middleware on
			// /api/control/* one-shot consumes it. Without this, the
			// rewriter would have to embed the agent's raw cvis_ token
			// (which the nonce middleware rejects) in the model's
			// conversation context.
			if cfg.CallerNonces == nil {
				audit("block", "caller_nonce_unavailable", "caller nonce cache not configured")
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: caller nonce cache not configured; refusing to embed agent token in control tool_use",
				}
			}
			nonce, mintErr := cfg.CallerNonces.Mint(req.Context(), cfg.AgentID, NonceTarget{
				Host:   v.Host,
				Method: v.Method,
				Path:   v.Path,
			})
			if mintErr != nil {
				audit("block", "caller_nonce_mint_failed", mintErr.Error())
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: caller nonce mint failed — " + mintErr.Error(),
				}
			}
			rewritten, _, rewriteOK, err := RewriteControlToolUse(tu, cfg.ControlBaseURL, nonce)
			if !rewriteOK {
				audit("block", "control_unavailable", "no control rewrite base URL configured")
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: control endpoint unavailable",
				}
			}
			if err != nil {
				audit("block", "control_rewriter_error", err.Error())
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: control endpoint rewrite refused — " + err.Error(),
				}
			}
			audit("rewrite", "clawvisor_control", v.Reason)
			trace(TraceEventControlRewrite,
				"host", v.Host,
				"method", v.Method,
				"path", v.Path,
				"nonce_prefix", nonce[:min(len(nonce), 14)],
				"rewrite_bytes", len(rewritten),
			)
			return conversation.ToolUseVerdict{
				Allowed:      true,
				RewriteInput: rewritten,
			}
		} else if controlToolUseMentionsEndpoint(tu, cfg.ControlBaseURL) {
			reason := "malformed_control_command"
			if cfg.CallerNonces != nil {
				nonce, mintErr := cfg.CallerNonces.Mint(req.Context(), cfg.AgentID, NonceTarget{
					Host:   ControlSyntheticHost,
					Method: "POST",
					Path:   "/api/control/failure",
				})
				if mintErr != nil {
					audit("block", "caller_nonce_mint_failed", mintErr.Error())
					return conversation.ToolUseVerdict{
						Allowed: false,
						Reason:  "Clawvisor: caller nonce mint failed — " + mintErr.Error(),
					}
				}
				if rewritten, ok, err := RewriteControlFailureToolUse(tu, cfg.ControlBaseURL, nonce, reason); ok {
					if err != nil {
						audit("block", "control_rewriter_error", err.Error())
						return conversation.ToolUseVerdict{
							Allowed: false,
							Reason:  "Clawvisor: control endpoint failure rewrite refused — " + err.Error(),
						}
					}
					audit("rewrite", "clawvisor_control_failure", "malformed control endpoint command")
					trace(TraceEventControlRewrite,
						"host", ControlSyntheticHost,
						"method", "POST",
						"path", "/api/control/failure",
						"failure_reason", reason,
						"nonce_prefix", nonce[:min(len(nonce), 14)],
						"rewrite_bytes", len(rewritten),
					)
					return conversation.ToolUseVerdict{
						Allowed:      true,
						RewriteInput: rewritten,
					}
				}
			} else {
				audit("block", "caller_nonce_unavailable", "caller nonce cache not configured")
			}
			audit("block", "control_rewriter_error", "control endpoint command must be a single foreground curl with no pipes, subshells, or extra shell commands")
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: control endpoint rewrite refused — use a single foreground curl to the control endpoint, with no pipes, subshells, redirects to output files, or extra shell commands",
			}
		}

		v = cfg.Inspector.Inspect(req.Context(), inspector.ToolUse{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: tu.Input,
		})
		trace(TraceEventInspectVerdict,
			"source", string(v.Source),
			"is_api_call", v.IsAPICall,
			"ambiguous", v.Ambiguous,
			"method", v.Method,
			"host", v.Host,
			"path", v.Path,
			"placeholders", v.Placeholders,
			"reason", v.Reason,
		)

		// If the only `autovault_…` substrings in this tool_use are too
		// short to be real vault references — test fixtures, prose
		// examples, doc snippets — there's no credential to mediate.
		// Downgrade to trigger-miss so the surrounding tool call (often
		// an Edit of a test file that mentions the literal) is evaluated
		// under normal authorization rather than refused as ambiguous.
		if v.Source != inspector.SourceTriggerMiss && inspector.AllPlaceholdersAreStubs(v.Placeholders) {
			audit("allow", "stub_placeholder", "placeholders below realistic length floor")
			v = inspector.Verdict{
				IsAPICall: false,
				Source:    inspector.SourceTriggerMiss,
				Reason:    "placeholders are stub-length (no real vault reference)",
			}
		}

		// Inspector says trigger missed (no autovault placeholder). There
		// is no credential rewrite to perform, but shared authorization
		// still sees ordinary tool_use calls such as Bash/Read.
		if v.Source == inspector.SourceTriggerMiss {
			readOnlyShellCommand := false
			sensitiveShellPath := false
			if toolnames.IsShellToolName(tu.Name) && readOnlyShellCommandsAllowed(tu.Name, cfg.AgentID, cfg.ToolRules) {
				if cmd := shellCommandFromInput(tu.Input); cmd != "" {
					readOnlyShellCommand, _ = inspector.IsReadOnlyBashCommand(cmd)
					if toolnames.SensitiveFileGuardEnabled(tu.Name, cfg.AgentID, cfg.ToolRules) {
						// A shell command that references a sensitive path
						// (SSH key, .env, cloud creds) must NOT ride the
						// generic trigger-miss pass-through or the
						// readonly_shell_pass_through bypass. Force
						// fall-through to task-scope matching and intent
						// verification.
						if tok, reason, hit := inspector.CommandReferencesSensitivePath(cmd); hit {
							sensitiveShellPath = true
							readOnlyShellCommand = false
							audit("block", "sensitive_path_in_read_only_shell", "command references sensitive path "+tok+" ("+reason+")")
						}
					}
				}
			}
			if cfg.CandidateTasks != nil || cfg.ToolRules != nil || cfg.EgressRules != nil || sensitiveShellPath {
				decisionInput := runtimedecision.AuthorizationInput{
					ToolUse:                tu,
					UserID:                 cfg.AgentUserID,
					AgentID:                cfg.AgentID,
					Posture:                cfg.Posture,
					CandidateTasks:         cfg.CandidateTasks,
					ToolRules:              cfg.ToolRules,
					EgressRules:            cfg.EgressRules,
					PreferredTaskID:        cfg.PreferredTaskID,
					IntentVerifier:         decisionIntentVerifier{inner: cfg.IntentVerifier},
					SkipIntentVerification: readOnlyShellCommand,
				}
				dec, err := runtimedecision.EvaluateAuthorization(req.Context(), decisionInput)
				if err != nil {
					audit("block", "decision_error", err.Error())
					return conversation.ToolUseVerdict{Allowed: false, Reason: "Clawvisor: authorization failed — " + err.Error()}
				}
				matchedTaskID = taskIDFromDecision(dec)
				trace(TraceEventDecision,
					"path", "trigger_miss",
					"kind", string(dec.Kind),
					"source", string(dec.Source),
					"reason", dec.Reason,
					"task_id", taskIDFromDecision(dec),
				)
				switch dec.Kind {
				case runtimedecision.VerdictAllow:
					audit("allow", string(dec.Source), dec.Reason)
					return conversation.ToolUseVerdict{Allowed: true}
				case runtimedecision.VerdictDeny:
					audit("block", string(dec.Source), dec.Reason)
					return conversation.ToolUseVerdict{Allowed: false, Reason: "Clawvisor: " + dec.Reason}
				case runtimedecision.VerdictNeedsApproval:
					// Codex's write_stdin with empty chars is the
					// harness polling a background shell for output —
					// equivalent to Claude Code's BashOutput. No
					// state change, no side effect. Pass through.
					if dec.Source == runtimedecision.SourceTaskScopeMissing && isShellPollTool(tu.Name, tu.Input) {
						audit("allow", "shell_poll_pass_through", "background-shell poll ("+tu.Name+")")
						trace(TraceEventDecision, "path", "trigger_miss", "kind", "allow", "source", "shell_poll_pass_through", "reason", "background-shell poll")
						return conversation.ToolUseVerdict{Allowed: true}
					}
					if dec.Source == runtimedecision.SourceTaskScopeMissing && readOnlyShellCommand {
						audit("allow", "readonly_shell_pass_through", "read-only shell command")
						trace(TraceEventDecision, "path", "trigger_miss", "kind", "allow", "source", "readonly_shell_pass_through", "reason", "read-only shell command", "cmd_preview", truncateForTrace(shellCommandFromInput(tu.Input), 200))
						return conversation.ToolUseVerdict{Allowed: true}
					}
					// Hold before rendering so the approval ID can be
					// embedded in the substitute message footer. The
					// agent's NEXT turn will carry that marker in
					// assistant history and let a bare "y"/"n" reply
					// resolve to this specific hold without the user
					// having to type the ID.
					var approvalID string
					if cfg.PendingApprovals != nil {
						held, err := cfg.PendingApprovals.Hold(req.Context(), PendingLiteApproval{
							UserID:         cfg.AgentUserID,
							AgentID:        cfg.AgentID,
							Provider:       provider,
							ConversationID: cfg.ConversationID,
							ToolUse:        tu,
							Inspector:      v,
							Fingerprint:    runtimedecision.Fingerprint(dec, decisionInput),
							Reason:         dec.Reason,
						})
						if err != nil {
							audit("block", "approval_hold_error", err.Error())
							return conversation.ToolUseVerdict{
								Allowed: false,
								Reason:  "Clawvisor: approval unavailable — " + err.Error(),
							}
						}
						if held.Evicted != nil {
							audit("block", "approval_evicted", "superseded pending approval "+held.Evicted.ID)
							// If an inline-task hold was evicted by this
							// commit, terminate its store.Task so the
							// dashboard doesn't keep showing a zombie
							// row that chat can no longer resolve.
							cleanupEvictedInlineTask(req.Context(), cfg, held.Evicted)
						}
						approvalID = held.Pending.ID
					}
					audit("block", string(dec.Source), dec.Reason)
					return conversation.ToolUseVerdict{
						Allowed:        false,
						Reason:         "Clawvisor: approval required — " + dec.Reason,
						SubstituteWith: approvalPrompt(tu, dec.Reason, approvalID),
					}
				}
			}
			// Record ordinary tool uses even when no credential trigger was
			// present so lite-proxy activity shows the agent's tool calls.
			audit("allow", "pass_through", "no credential trigger")
			return conversation.ToolUseVerdict{Allowed: true}
		}
		if v.Ambiguous || !v.IsAPICall {
			audit("block", "ambiguous", v.Reason)
			// ContinueWithToolResult preserves the agent's actual
			// tool_use in conversation history and feeds the rejection
			// back as a synthetic tool_result — the canonical Anthropic
			// shape for "your tool call failed, here's why." The model's
			// own emitted input is right above the synthetic user turn,
			// so it can see exactly what shape got rejected. The
			// SubstituteWith fallback is what gets rendered if the
			// handler can't perform the continuation call (older
			// provider, recursion bound reached, upstream outage).
			reason := "Clawvisor: ambiguous credentialed call refused — " + v.Reason
			return conversation.ToolUseVerdict{
				Allowed:                false,
				Reason:                 reason,
				ContinueWithToolResult: reason,
			}
		}

		// Authorization boundary: the validator's `Host` is a candidate.
		// The authoritative source is the placeholder's bound service
		// host allowlist. Look it up and run BoundaryCheck. Mismatch =
		// fail closed.
		boundaryReason, boundaryOK := boundaryCheckVerdict(req, cfg, v)
		trace(TraceEventBoundaryCheck,
			"ok", boundaryOK,
			"reason", boundaryReason,
			"placeholders", v.Placeholders,
			"verdict_host", v.Host,
		)
		if !boundaryOK {
			audit("block", "boundary_check_failed", boundaryReason)
			reason := "Clawvisor: target host outside placeholder bound-service — " + boundaryReason
			return conversation.ToolUseVerdict{
				Allowed:                false,
				Reason:                 reason,
				ContinueWithToolResult: reason,
			}
		}

		decisionHandled := false
		if cfg.CandidateTasks != nil || cfg.ToolRules != nil || cfg.EgressRules != nil {
			resolved := ResolvedAction{}
			if cfg.Catalog != nil {
				resolved, _ = cfg.Catalog.Resolve(v.Host, v.Method, v.Path)
			}
			decisionInput := runtimedecision.AuthorizationInput{
				ToolUse:         tu,
				UserID:          cfg.AgentUserID,
				AgentID:         cfg.AgentID,
				Posture:         cfg.Posture,
				Target:          runtimedecision.TargetRequest{Host: v.Host, Method: v.Method, Path: v.Path},
				Service:         resolved.ServiceID,
				Action:          resolved.ActionID,
				CandidateTasks:  cfg.CandidateTasks,
				ToolRules:       cfg.ToolRules,
				EgressRules:     cfg.EgressRules,
				PreferredTaskID: cfg.PreferredTaskID,
				IntentVerifier:  decisionIntentVerifier{inner: cfg.IntentVerifier},
			}
			dec, err := runtimedecision.EvaluateAuthorization(req.Context(), decisionInput)
			if err != nil {
				audit("block", "decision_error", err.Error())
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: authorization failed — " + err.Error(),
				}
			}
			matchedTaskID = taskIDFromDecision(dec)
			trace(TraceEventDecision,
				"path", "credentialed",
				"kind", string(dec.Kind),
				"source", string(dec.Source),
				"reason", dec.Reason,
				"service", resolved.ServiceID,
				"action", resolved.ActionID,
				"task_id", taskIDFromDecision(dec),
			)
			switch dec.Kind {
			case runtimedecision.VerdictAllow:
				// Continue to credential rewrite below.
				decisionHandled = true
			case runtimedecision.VerdictDeny:
				audit("block", string(dec.Source), dec.Reason)
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: " + dec.Reason,
				}
			case runtimedecision.VerdictNeedsApproval:
				// Hold first so the assigned approval ID can be
				// embedded in the substitute prompt footer; see the
				// trigger-miss branch above for the same pattern.
				var approvalID string
				if cfg.PendingApprovals != nil {
					held, err := cfg.PendingApprovals.Hold(req.Context(), PendingLiteApproval{
						UserID:         cfg.AgentUserID,
						AgentID:        cfg.AgentID,
						Provider:       provider,
						ConversationID: cfg.ConversationID,
						ToolUse:        tu,
						Inspector:      v,
						Fingerprint:    runtimedecision.Fingerprint(dec, decisionInput),
						Reason:         dec.Reason,
					})
					if err != nil {
						audit("block", "approval_hold_error", err.Error())
						return conversation.ToolUseVerdict{
							Allowed: false,
							Reason:  "Clawvisor: approval unavailable — " + err.Error(),
						}
					}
					if held.Evicted != nil {
						audit("block", "approval_evicted", "superseded pending approval "+held.Evicted.ID)
						// See above: terminate evicted inline-task rows so
						// the dashboard doesn't strand them.
						cleanupEvictedInlineTask(req.Context(), cfg, held.Evicted)
					}
					approvalID = held.Pending.ID
				}
				audit("block", string(dec.Source), dec.Reason)
				return conversation.ToolUseVerdict{
					Allowed:        false,
					Reason:         "Clawvisor: approval required — " + dec.Reason,
					SubstituteWith: approvalPrompt(tu, dec.Reason, approvalID),
				}
			}
		}

		// Task-scope authorization: reverse-resolve the (host, method,
		// path) to (service, action), then check against the agent's
		// active tasks. Skipping is audited (in case of misconfig) but
		// not blocking — v0 leaves task-scope as opt-in until product
		// surfaces (always_ask / approval queue) are wired in #33.
		if !decisionHandled && cfg.Catalog != nil && cfg.TaskScope != nil {
			if resolved, ok := cfg.Catalog.Resolve(v.Host, v.Method, v.Path); ok {
				dec := cfg.TaskScope.Check(req.Context(), cfg.AgentUserID, cfg.AgentID, resolved.ServiceID, resolved.ActionID)
				if !dec.Allowed {
					audit("block", "task_scope_denied", dec.Reason)
					return conversation.ToolUseVerdict{
						Allowed: false,
						Reason:  "Clawvisor: no active task scope covers " + resolved.ServiceID + "." + resolved.ActionID + " — " + dec.Reason,
					}
				}
				matchedTaskID = dec.TaskID
				// Intent verification: when the matched TaskAction's
				// Verification mode opts in (strict | lenient | empty)
				// and an IntentVerifier is configured, the LLM compares
				// the request's params + tool_use shape to the matched
				// expected_use. Off mode and missing verifier skip silently.
				if reason, ok := runIntentVerify(req.Context(), cfg, dec, resolved, tu); !ok {
					audit("block", "intent_verification_failed", reason)
					return conversation.ToolUseVerdict{
						Allowed: false,
						Reason:  "Clawvisor: intent verification refused " + resolved.ServiceID + "." + resolved.ActionID + " — " + reason,
					}
				}
			}
			// Catalog miss: log via audit reason field but don't block.
			// The fact that the (host, method, path) didn't resolve to a
			// known (service, action) is an inspector or catalog gap, not
			// an attack signal — the BoundaryCheck above already constrained
			// the host to the placeholder's bound-service allowlist.
		}

		// Mint a per-tool nonce that stands in for the agent's bearer
		// token in the rewritten tool_use's X-Clawvisor-Caller header.
		// The nonce is bound to (agent, host, method, path); the
		// resolver consumes it one-shot on the matching call. Failure
		// to mint (cache misconfigured or backend down) fails closed —
		// we won't embed the raw agent token in the conversation as a
		// fallback.
		if cfg.CallerNonces == nil {
			audit("block", "caller_nonce_unavailable", "caller nonce cache not configured")
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: caller nonce cache not configured; refusing to embed agent token in tool_use",
			}
		}
		nonce, mintErr := cfg.CallerNonces.Mint(req.Context(), cfg.AgentID, NonceTarget{
			Host:   v.Host,
			Method: v.Method,
			Path:   v.Path,
		})
		if mintErr != nil {
			audit("block", "caller_nonce_mint_failed", mintErr.Error())
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: caller nonce mint failed — " + mintErr.Error(),
			}
		}
		opts := cfg.RewriteOpts
		opts.CallerToken = nonce
		rewritten, err := inspector.Rewrite(inspector.ToolUse{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: tu.Input,
		}, v, opts)
		if err != nil {
			audit("block", "rewriter_error", err.Error())
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: rewriter refused — " + err.Error(),
			}
		}
		audit("rewrite", "success", v.Reason)
		trace(TraceEventRewriteApplied,
			"host", v.Host,
			"method", v.Method,
			"path", v.Path,
			"placeholders", v.Placeholders,
			"nonce_prefix", nonce[:min(len(nonce), 14)],
			"rewrite_bytes", len(rewritten),
		)
		return conversation.ToolUseVerdict{
			Allowed:      true,
			RewriteInput: rewritten,
		}
	}
}

// coalesceFromCaptures builds the single PendingLiteApproval covering
// every tool_use in a turn. The first approval-needing capture becomes
// the primary (its decision context is mapped to the singular
// ToolUse/Inspector/Fingerprint/Reason fields the rest of the codebase
// already understands). PrimaryIndex records where the primary sat in
// the original turn, so AllHolds() — and the release path that emits
// from it — keep the model's tool_use order intact. Reordering would
// break dependent sequences like Bash producing stdout that a
// following WebFetch consumes.
func coalesceFromCaptures(captures []evalCapture) PendingLiteApproval {
	primaryIdx := -1
	for i, c := range captures {
		if c.Kind == HeldKindApproval {
			primaryIdx = i
			break
		}
	}
	if primaryIdx < 0 {
		// shouldCoalesceTurn would have returned false; treat as
		// defensive guard so callers don't have to re-check.
		primaryIdx = 0
	}
	primary := captures[primaryIdx]
	pending := PendingLiteApproval{
		ToolUse:      primary.Use,
		Inspector:    primary.Inspector,
		Fingerprint:  primary.Fingerprint,
		Reason:       primary.Reason,
		PrimaryIndex: primaryIdx,
	}
	pending.Additional = make([]HeldToolUse, 0, len(captures)-1)
	for i, c := range captures {
		if i == primaryIdx {
			continue
		}
		pending.Additional = append(pending.Additional, HeldToolUse{
			ToolUse:     c.Use,
			Kind:        c.Kind,
			Inspector:   c.Inspector,
			Fingerprint: c.Fingerprint,
			Reason:      c.Reason,
		})
	}
	return pending
}

// ambiguousRefusalGuidance produces the substitute message the model
// sees when the inspector refused a credentialed call as ambiguous.
// The model needs actionable instructions on how to rewrite the call
// in a shape Clawvisor can mediate — otherwise it retries the same
// shape and ends up in a loop, or worse, copies a fragment back into
// the conversation and gets stuck.
func ambiguousRefusalGuidance(tu conversation.ToolUse, v inspector.Verdict) string {
	preview := conversation.MakeToolInputPreview(tu.Input)
	var b strings.Builder
	b.WriteString("Clawvisor refused this credentialed call: ")
	b.WriteString(v.Reason)
	b.WriteString(".")
	if tu.Name != "" {
		b.WriteString("\n\nTool: `")
		b.WriteString(tu.Name)
		b.WriteString("`")
	}
	if preview != "" {
		b.WriteString("\nInput: ")
		b.WriteString(preview)
	}
	// Tailored guidance based on the parser's specific objection.
	reason := strings.ToLower(v.Reason)
	switch {
	case strings.Contains(reason, "shell metacharacter"):
		b.WriteString("\n\nRewrite the command as a single curl invocation with no pipes, redirects, command chaining (`|`, `;`, `&&`, `>`, `2>&1`), command substitution (`$(...)`, backticks), or subshells. Clawvisor needs to parse the curl shape to inject credentials safely. If you need to filter or post-process the response, run a separate tool call after the curl returns.")
	case strings.Contains(reason, "unknown curl flag"):
		b.WriteString("\n\nThe curl flag isn't on Clawvisor's allowlist (only common safe flags like `-s`, `-S`, `-f`, `-i`, `-A`, `-o`, `--max-time` are accepted; `-L`, `-k`, `-x`, `-d`, `--data*`, `-T`, `-F` are refused). Rewrite without that flag.")
	case strings.Contains(reason, "expected exactly one positional URL"):
		b.WriteString("\n\nUse exactly one URL positional argument. If you need to call multiple endpoints, run separate tool calls.")
	case strings.Contains(reason, "placeholder not in"):
		b.WriteString("\n\nThe credential placeholder must appear in an HTTP header (e.g. `-H 'Authorization: Bearer autovault_…'`). Body, query, or non-header locations are not yet supported for rewrite.")
	default:
		b.WriteString("\n\nRewrite the call in the simplest shape Clawvisor can mediate: a single curl invocation with `-H 'Authorization: Bearer <autovault_placeholder>'` and one URL positional argument. No pipes, redirects, or command chaining.")
	}
	return b.String()
}

// approvalPrompt renders the agent-facing message that substitutes for a
// paused tool call. When approvalID is non-empty, the InlineApprovalIDMarker
// footer is appended so subsequent turns can disambiguate which hold a bare
// "y"/"n" reply targets — important when one agent's transcript contains
// multiple pending prompts, or when several agents share a Clawvisor token
// and only the per-transcript marker reliably identifies the right hold.
func approvalPrompt(tu conversation.ToolUse, reason, approvalID string) string {
	preview := conversation.MakeToolInputPreview(tu.Input)
	var b strings.Builder
	b.WriteString("Clawvisor paused this tool call for approval.")
	if tu.Name != "" {
		b.WriteString("\n\nTool: `")
		b.WriteString(tu.Name)
		b.WriteString("`")
	}
	if reason != "" {
		b.WriteString("\nReason: ")
		b.WriteString(reason)
	}
	if preview != "" {
		b.WriteString("\nInput: ")
		b.WriteString(preview)
	}
	b.WriteString("\n\nReply `yes` or `y` to run this tool call, `no` or `n` to block it, or `task` to instruct the agent to include this in a task definition for approval.")
	b.WriteString(approvalIDFooter(approvalID))
	return b.String()
}

// coalescedApprovalPrompt renders the prompt for a hold that covers
// multiple tool_uses in one turn. Offers approve/deny/task — the same
// three verbs as the single-tool prompt. "task" against a coalesced
// hold generates a task-definition prompt whose expected_tools
// enumerates every distinct tool name in the batch (see
// taskCreationPromptForHolds), so the user can promote the whole
// batch into a durable scope in one gesture instead of approving
// each call individually.
//
// The kinds slice parallels uses: each entry tags whether that use was
// the trigger for approval or held alongside (auto-allow / auto-rewrite).
func coalescedApprovalPrompt(uses []HeldToolUse) string {
	var b strings.Builder
	b.WriteString("Clawvisor paused this turn for approval (")
	b.WriteString(strconv.Itoa(len(uses)))
	b.WriteString(" tool calls).")
	for i, held := range uses {
		b.WriteString("\n\n")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		if name := strings.TrimSpace(held.ToolUse.Name); name != "" {
			b.WriteString("`")
			b.WriteString(name)
			b.WriteString("`")
		} else {
			b.WriteString("(unnamed tool)")
		}
		switch held.Kind {
		case HeldKindApproval:
			if reason := strings.TrimSpace(held.Reason); reason != "" {
				b.WriteString(" — approval required: ")
				b.WriteString(reason)
			} else {
				b.WriteString(" — approval required")
			}
		case HeldKindAllow:
			b.WriteString(" — held alongside (would auto-allow on its own)")
		case HeldKindRewrite:
			b.WriteString(" — held alongside (would auto-allow with credential rewrite on its own)")
		}
		if preview := conversation.MakeToolInputPreview(held.ToolUse.Input); preview != "" {
			b.WriteString("\n   Input: ")
			b.WriteString(preview)
		}
	}
	b.WriteString("\n\nReply `yes` or `y` to approve all calls and run them in order, `no` or `n` to deny the whole turn, or `task` to scope this work under a Clawvisor task that covers every call above.")
	return b.String()
}

func taskCreationPrompt(tu conversation.ToolUse) string {
	return taskCreationPromptForHolds([]HeldToolUse{{ToolUse: tu, Kind: HeldKindApproval}})
}

// taskCreationPromptForHolds renders the task-creation prompt for one
// or more held tool_uses. When len(holds) == 1 the output is
// byte-identical to the legacy single-tool taskCreationPrompt — the
// inline-task flow on a single hold is unchanged. When len(holds) > 1
// (coalesced hold), `expected_tools` enumerates every distinct tool
// name in the batch so the generated task scope covers every held
// call. Without this, typing "task" on a coalesced approval prompt
// would scope only the primary tool and leave sibling reviewed calls
// to re-prompt on the next retry.
func taskCreationPromptForHolds(holds []HeldToolUse) string {
	if len(holds) == 0 {
		return ""
	}
	// Deduplicate by tool name; keep insertion order so the rendered
	// expected_tools mirrors the model's emit order (matters for
	// dependent sequences readers will recognize). The why for a
	// duplicated tool name comes from the FIRST tool_use of that
	// name — taskToolWhy already produces a description broad enough
	// to cover sibling calls (e.g. "Run shell commands needed for
	// the task, including writes AND verification reads").
	seen := map[string]bool{}
	expected := make([]map[string]any, 0, len(holds))
	for _, held := range holds {
		name := strings.TrimSpace(held.ToolUse.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		expected = append(expected, map[string]any{
			"tool_name": name,
			"why":       taskToolWhy(held.ToolUse),
		})
	}
	if len(expected) == 0 {
		return ""
	}
	payload := map[string]any{
		"purpose":                  "Describe the user-visible task you are trying to complete, including why this tool access is needed.",
		"expected_tools":           expected,
		"intent_verification_mode": "strict",
		"expires_in_seconds":       600,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	// The user just typed "task" at the inline prompt — they are
	// definitionally at the chat surface. Pass ?surface=inline so the
	// proxy holds the yes/no gesture inline rather than routing
	// to the dashboard's notification queue.
	//
	// Use the single-curl `--data @- <<JSON` shape. The proxy DOES
	// accept a cat-heredoc-to-file then curl --data @file pattern, but
	// it's strictly more error-prone — keep the prompt to one shape.
	//
	// RUN IT IN THE FOREGROUND. The task-creation curl must block on
	// my decision; backgrounding it makes the agent proceed before
	// approval lands. Avoid Codex-specific parameter names in the
	// prompt — naming yield_time_ms tends to make the model set it
	// to a small default. The proxy clamps the parameter to a safe
	// minimum as a belt-and-suspenders fallback.
	return "Please request a Clawvisor task for this work using the proxy-lite control endpoint. The user will need to approve the task after it is created. Your next assistant message must be exactly one shell tool_use that runs the foreground curl below, then waits for the result. Do not print, describe, or summarize the JSON in chat. Do not answer with a markdown code block. Do not background it, do not split it across shells, do not poll a backgrounded session. POST the task definition to `https://clawvisor.local/control/tasks?surface=inline` so I can approve it without leaving the chat. Include the blocked action and any related tools or commands you expect to need. For normal temporary work, omit `lifetime` or set `\"lifetime\":\"session\"` with `expires_in_seconds`. Use `\"lifetime\":\"standing\"` only when the user explicitly wants persistent permission; standing tasks must not include `expires_in_seconds`.\n\nRun this exact command as one foreground shell tool call (JSON via `--data @-` heredoc, no intermediate file, no trailing `&`, no `nohup`):\n\ncurl -sS -X POST 'https://clawvisor.local/control/tasks?surface=inline' \\\n  -H 'Content-Type: application/json' \\\n  --data @- <<'JSON'\n" + string(raw) + "\nJSON"
}

// taskToolWhy renders a default `why` for the model when the blocked
// tool is being lifted into a fresh task definition. The text is
// intentionally expansive about read/verify follow-ups so the LLM
// intent verifier (which compares each tool_use to the matched
// action's `why`) doesn't refuse the natural after-write inspect
// commands an agent does to confirm its own work.
func taskToolWhy(tu conversation.ToolUse) string {
	switch strings.TrimSpace(tu.Name) {
	case "Bash", "bash", "exec_command":
		if command := toolInputString(tu.Input, "command", "cmd"); command != "" {
			return "Run shell commands needed for the task, including writes AND verification reads (ls, wc, cat, stat) against the resulting files. Initial command: " + command
		}
	case "Read":
		if path := toolInputString(tu.Input, "file_path", "path"); path != "" {
			return "Read files needed for the task, including: " + path
		}
	case "Write", "Edit", "NotebookEdit":
		if path := toolInputString(tu.Input, "file_path", "path"); path != "" {
			return "Create, modify, and read back files needed for the task (verifying writes is part of the workflow), including: " + path
		}
	case "WebFetch", "WebSearch":
		if target := toolInputString(tu.Input, "url", "query"); target != "" {
			return "Use web access needed for the task, including: " + target
		}
	}
	return "Use this tool for the requested task. Include a concise description of the command pattern, file path, URL, or operation; if writing or modifying, also cover the read-back verification you will do afterward."
}

func toolInputString(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 {
		return ""
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := input[key].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return v
			}
		}
	}
	return ""
}

type decisionIntentVerifier struct {
	inner IntentVerifier
}

func (v decisionIntentVerifier) Verify(ctx context.Context, req runtimedecision.IntentVerifyRequest) (*runtimedecision.IntentVerdict, error) {
	if v.inner == nil {
		return nil, nil
	}
	verdict, err := v.inner.Verify(ctx, IntentVerifyRequest{
		TaskPurpose: req.TaskPurpose,
		ExpectedUse: req.ExpectedUse,
		Service:     req.Service,
		Action:      req.Action,
		Params:      req.Params,
		Reason:      req.Reason,
		TaskID:      req.TaskID,
		Lenient:     req.Lenient,
	})
	if err != nil || verdict == nil {
		return nil, err
	}
	return &runtimedecision.IntentVerdict{
		Allow:       verdict.Allow,
		Explanation: verdict.Explanation,
	}, nil
}

// auditAgentForCfg builds a minimal *store.Agent for the audit emitter
// from the postprocess config. The emitter only reads UserID and ID; we
// avoid an extra DB lookup by synthesizing the struct.
func auditAgentForCfg(cfg PostprocessConfig) *store.Agent {
	if cfg.Audit == nil || cfg.AgentID == "" || cfg.AgentUserID == "" {
		return nil
	}
	return &store.Agent{ID: cfg.AgentID, UserID: cfg.AgentUserID}
}

func readOnlyShellCommandsAllowed(toolName, agentID string, rules []*store.RuntimePolicyRule) bool {
	global := true
	agent := (*bool)(nil)
	for _, rule := range rules {
		if rule == nil || !rule.Enabled || !toolnames.IsReadOnlyShellSettingRule(rule) || !toolnames.ToolNamesSameClass(rule.ToolName, toolName) {
			continue
		}
		allowed := strings.EqualFold(strings.TrimSpace(rule.Action), "allow")
		if rule.AgentID != nil {
			if strings.TrimSpace(*rule.AgentID) == strings.TrimSpace(agentID) {
				v := allowed
				agent = &v
			}
			continue
		}
		global = allowed
	}
	if agent != nil {
		return *agent
	}
	return global
}

// isShellPollTool reports whether a tool_use is a harness poll on a
// background shell — read-equivalent and worth passing through. The
// canonical case is Codex's `write_stdin` with empty `chars`, which
// the harness emits continuously while a backgrounded `exec_command`
// is running. Non-empty `chars` is actual input typed into a shell
// (potentially mutating); stay strict.
func isShellPollTool(name string, raw json.RawMessage) bool {
	if name != "write_stdin" {
		return false
	}
	if len(raw) == 0 {
		return false
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return false
	}
	chars, ok := input["chars"].(string)
	if !ok {
		return false
	}
	return strings.TrimSpace(chars) == ""
}

// shellCommandFromInput extracts the command string from a shell-tool
// input JSON. Claude Code's Bash uses `command`; Codex's exec_command
// uses `cmd`. Returns "" when neither is present or non-string.
func shellCommandFromInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return ""
	}
	if v, ok := input["cmd"].(string); ok && v != "" {
		return v
	}
	if v, ok := input["command"].(string); ok {
		return v
	}
	return ""
}

// taskIDFromDecision extracts the matched task's ID from a decision,
// returning "" when there is no associated task. Trace-only helper.
func taskIDFromDecision(dec runtimedecision.AuthorizationDecision) string {
	if dec.Task == nil {
		return ""
	}
	return dec.Task.ID
}

// redactPlaceholderForReason returns the placeholder's prefix +
// length suffix — enough for operators to identify which placeholder
// was missing vs. which actually exists in the DB, without exposing
// the full random suffix in audit reasons that may surface in UIs or
// logs shared more broadly than the placeholder itself.
func redactPlaceholderForReason(ph string) string {
	const head = 18 // long enough to keep `autovault_<svc>_…`
	if len(ph) <= head {
		return ph
	}
	return ph[:head] + "…(" + strconv.Itoa(len(ph)) + " chars)"
}

// boundaryCheckVerdict validates the inspector's claimed host against
// the bound-service allowlist of every placeholder it found.
func boundaryCheckVerdict(req *http.Request, cfg PostprocessConfig, v inspector.Verdict) (string, bool) {
	if cfg.Store == nil {
		return "no store configured for boundary check", false
	}
	if cfg.AgentUserID == "" || cfg.AgentID == "" {
		return "no agent context for boundary check", false
	}
	if len(v.Placeholders) == 0 {
		return "verdict missing placeholder for boundary lookup", false
	}
	for _, ph := range v.Placeholders {
		rec, err := cfg.Store.GetRuntimePlaceholder(req.Context(), ph)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return "placeholder not registered: " + redactPlaceholderForReason(ph), false
			}
			return "store error: " + err.Error(), false
		}
		if reason, ok := ValidateRuntimePlaceholderAccess(req.Context(), cfg.Store, rec, cfg.AgentUserID, cfg.AgentID, time.Now().UTC()); !ok {
			return reason + " (placeholder=" + redactPlaceholderForReason(ph) + ")", false
		}
		hosts, boundReason := RuntimePlaceholderBoundHosts(req.Context(), cfg.Store, rec)
		if len(hosts) == 0 {
			return boundReason, false
		}
		if ok, reason := inspector.BoundaryCheck(v, hosts); !ok {
			return reason, false
		}
	}
	return "", true
}

// runIntentVerify runs LLM intent verification when the matched TaskAction
// opts in. Returns (reason, ok). ok=false on a refusal verdict; ok=true when
// the verifier was not consulted (off mode / missing dep) or returned Allow.
//
// Verification mode mapping (matches gateway behavior):
//   - "off"             → skip verification, allow.
//   - "lenient"         → call verifier with Lenient=true.
//   - "strict" / empty  → call verifier with Lenient=false.
//
// On verifier error we fail-open (audit will record), matching the gateway's
// behavior so a transient LLM outage doesn't block tool use; #37 will tighten
// this to fail-closed once the circuit breaker is in place.
func runIntentVerify(ctx context.Context, cfg PostprocessConfig, dec TaskScopeDecision, resolved ResolvedAction, tu conversation.ToolUse) (string, bool) {
	if cfg.IntentVerifier == nil || dec.MatchedAction == nil {
		return "", true
	}
	mode := dec.MatchedAction.Verification
	if mode == "off" {
		return "", true
	}
	purpose := ""
	if dec.MatchedTask != nil {
		purpose = dec.MatchedTask.Purpose
	}
	var params map[string]any
	if len(tu.Input) > 0 {
		_ = json.Unmarshal(tu.Input, &params)
	}
	verdict, err := cfg.IntentVerifier.Verify(ctx, IntentVerifyRequest{
		TaskPurpose: purpose,
		ExpectedUse: dec.MatchedAction.ExpectedUse,
		Service:     resolved.ServiceID,
		Action:      resolved.ActionID,
		Params:      params,
		Reason:      "lite-proxy tool_use " + tu.Name,
		TaskID:      dec.TaskID,
		Lenient:     mode == "lenient",
	})
	if err != nil {
		// Circuit-breaker outage signals fail-closed: until the verifier
		// recovers, we refuse rather than allow tool_use without scope
		// validation. Other errors (timeouts, transient network failures)
		// fail-open to match the gateway's behavior so a single hiccup
		// doesn't strand the agent.
		if errors.Is(err, ErrCircuitOpen) {
			return "verifier_circuit_open", false
		}
		return fmt.Sprintf("verifier_error: %s", err.Error()), true
	}
	if verdict == nil {
		// Verifier disabled at config level — treat as off.
		return "", true
	}
	if verdict.Allow {
		return verdict.Explanation, true
	}
	return verdict.Explanation, false
}

// matchByRoute resolves the response rewriter that pairs with the inbound
// route. The conversation.ResponseRegistry's MatchesResponse depends on
// the request's host (for runtime-proxy CONNECT use); for lite-proxy we
// dispatch by route path instead.
func matchByRoute(req *http.Request, registry *conversation.ResponseRegistry) conversation.ResponseRewriter {
	if registry == nil || req == nil || req.URL == nil {
		return nil
	}
	parsers := conversation.DefaultRegistry()
	parser := parsers.ParserForRoute(req.URL.Path)
	if parser == nil {
		return nil
	}
	provider := parser.Name()
	return registry.ForProvider(provider)
}

func matchByRouteStreaming(req *http.Request, registry *conversation.ResponseRegistry) conversation.StreamingResponseRewriter {
	if registry == nil || req == nil || req.URL == nil {
		return nil
	}
	parsers := conversation.DefaultRegistry()
	parser := parsers.ParserForRoute(req.URL.Path)
	if parser == nil {
		return nil
	}
	return registry.ForProviderStreaming(parser.Name())
}

func PostprocessStream(
	ctx context.Context,
	req *http.Request,
	r io.Reader,
	w io.Writer,
	contentType string,
	cfg PostprocessConfig,
) (PostprocessResult, error) {
	if cfg.Inspector == nil {
		_, err := io.Copy(w, r)
		return PostprocessResult{SkippedReason: "no inspector configured"}, err
	}

	registry := cfg.ResponseRegistry
	if registry == nil {
		registry = conversation.DefaultResponseRegistry()
	}

	streamingRewriter := matchByRouteStreaming(req, registry)
	if streamingRewriter == nil {
		_, err := io.Copy(w, r)
		return PostprocessResult{SkippedReason: "no streaming rewriter for route"}, err
	}

	provider := streamingRewriter.Name()

	// First-turn routing notice. Wrap the destination so the per-event
	// SSE state machine in the rewriter emits through an injector
	// that prepends the notice block at index 0 and shifts the rest
	// by +1. Mirrors the buffered path's PrependAssistantNotice call
	// in the handler — kept here (not in the handler) so the rewriter
	// itself doesn't need to grow a notice parameter, and so any
	// future streaming caller picks up the behavior automatically.
	if cfg.FirstTurnNotice != "" {
		shape := conversation.DetectStreamShape(req, provider)
		noticeW := conversation.NewStreamingFirstTurnNoticeWriter(w, shape, cfg.FirstTurnNotice)
		// The injector buffers partial SSE events; ensure trailing
		// state flushes when StreamRewrite returns. Closer-only
		// callers (Close()-implementing wrappers) flush; the no-op
		// case (StreamShapeUnknown / blank notice) returns dest
		// unchanged and is not closeable.
		if closer, ok := noticeW.(io.Closer); ok {
			defer func() { _ = closer.Close() }()
		}
		w = noticeW
	}

	auditAgent := auditAgentForCfg(cfg)

	originalPendingApprovals := cfg.PendingApprovals
	holdSink := &capturedHoldSink{}
	if originalPendingApprovals != nil {
		cfg.PendingApprovals = newHoldCapturingApprovalCache(originalPendingApprovals, holdSink)
	}
	auditSink := &capturedAuditSink{}
	var captures []evalCapture

	innerEval := newToolUseEvaluator(req, cfg, provider, auditSink)

	eval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		holdsBefore, auditsBefore := 0, 0
		if holdSink != nil {
			holdsBefore = len(holdSink.holds)
		}
		if auditSink != nil {
			auditsBefore = len(auditSink.entries)
		}
		v := innerEval(tu)
		c := evalCapture{Use: tu, Kind: classifyVerdict(v)}
		if holdSink != nil && len(holdSink.holds) > holdsBefore {
			h := holdSink.holds[len(holdSink.holds)-1]
			c.HoldID = h.Pending.ID
			c.Stage = h.Pending.Stage
			c.Inspector = h.Pending.Inspector
			c.Fingerprint = h.Pending.Fingerprint
			c.Reason = h.Pending.Reason
		} else if auditSink != nil && len(auditSink.entries) > auditsBefore {
			last := auditSink.entries[len(auditSink.entries)-1]
			c.Inspector = last.Verdict
			c.Reason = last.Reason
		}
		if auditSink != nil && len(auditSink.entries) > auditsBefore {
			c.TaskID = auditSink.entries[len(auditSink.entries)-1].TaskID
		}
		captures = append(captures, c)
		return v
	}

	streamResult, err := streamingRewriter.StreamRewrite(ctx, r, w)
	if err != nil {
		return PostprocessResult{}, err
	}
	if len(streamResult.ToolUses) == 0 {
		return PostprocessResult{
			ContentType: contentType,
		}, nil
	}

	var decisions []conversation.ToolUseDecisionRecord
	anyBlocked := false
	anyRewritten := false
	rewrittenInput := map[string]json.RawMessage{}

	for _, tu := range streamResult.ToolUses {
		v := eval(tu)
		decisions = append(decisions, conversation.ToolUseDecisionRecord{
			ToolUse:          tu,
			Verdict:          v,
			ToolInputPreview: conversation.MakeToolInputPreview(tu.Input),
		})
		if !v.Allowed {
			anyBlocked = true
		}
		if v.Allowed && len(v.RewriteInput) > 0 {
			rewrittenInput[tu.ID] = v.RewriteInput
			anyRewritten = true
		}
	}

	if originalPendingApprovals != nil && shouldCoalesceTurn(captures) {
		coalesced := coalesceFromCaptures(captures)
		coalesced.UserID = cfg.AgentUserID
		coalesced.AgentID = cfg.AgentID
		coalesced.Provider = provider
		coalesced.ConversationID = cfg.ConversationID
		held, holdErr := originalPendingApprovals.Hold(req.Context(), coalesced)
		if holdErr == nil {
			if held.Evicted != nil {
				if cfg.Audit != nil && auditAgent != nil && len(captures) > 0 {
					first := captures[0]
					cfg.Audit.LogToolUseInspected(req.Context(), auditAgent, cfg.RequestID, first.Use, first.Inspector, "block", "approval_evicted", "superseded pending approval "+held.Evicted.ID, first.TaskID)
				}
				cleanupEvictedInlineTask(req.Context(), cfg, held.Evicted)
			}
			emitCoalescedPendingAuditRows(req.Context(), cfg, auditAgent, captures, held.Pending.ID)

			coalescedPrompt := coalescedApprovalPrompt(held.Pending.AllHolds())
			if err := writeProviderBlockedPrompt(w, provider, streamResult, coalescedPrompt, streamingBlockedPromptIndex(provider, streamResult, captures)); err != nil {
				return PostprocessResult{}, err
			}

			return PostprocessResult{
				ContentType: contentType,
				Rewritten:   true,
				Decisions:   decisions,
			}, nil
		}
	}

	if replayErr := replayBufferedHolds(req.Context(), cfg, originalPendingApprovals, holdSink, auditAgent, captures); replayErr != nil {
		flushBufferedAudit(req.Context(), cfg, auditAgent, auditSink)
		return PostprocessResult{
			SkippedReason: "approval hold storage failed: " + replayErr.Error(),
		}, nil
	}
	flushBufferedAudit(req.Context(), cfg, auditAgent, auditSink)

	var continuationResults []conversation.ContinuationToolResult
	for _, dec := range decisions {
		if dec.Verdict.ContinueWithToolResult != "" {
			continuationResults = append(continuationResults, conversation.ContinuationToolResult{
				ToolUseID: dec.ToolUse.ID,
				Content:   dec.Verdict.ContinueWithToolResult,
			})
		}
	}

	if len(continuationResults) > 0 {
		return PostprocessResult{
			ContentType:             contentType,
			Rewritten:               true,
			Decisions:               decisions,
			ContinuationToolResults: continuationResults,
			AssistantTurn:           streamResult.AssistantTurn,
			StreamingProvider:       provider,
			StreamingResult:         streamResult,
		}, nil
	}

	if anyBlocked {
		subText := conversation.BlockedReasonText(decisions)
		if strings.TrimSpace(subText) == "" {
			subText = "Tool use was blocked by the Clawvisor proxy."
		}
		if err := writeProviderBlockedPrompt(w, provider, streamResult, subText, streamingBlockedPromptIndex(provider, streamResult, captures)); err != nil {
			return PostprocessResult{}, err
		}
	} else {
		if err := writeProviderToolUses(w, provider, streamResult, streamResult.ToolUses, rewrittenInput); err != nil {
			return PostprocessResult{}, err
		}
		if err := writeProviderStop(w, provider, streamResult); err != nil {
			return PostprocessResult{}, err
		}
	}

	return PostprocessResult{
		ContentType: contentType,
		Rewritten:   anyRewritten || anyBlocked,
		Decisions:   decisions,
	}, nil
}

func streamingBlockedPromptIndex(provider conversation.Provider, result conversation.StreamingRewriteResult, captures []evalCapture) int {
	if provider == conversation.ProviderAnthropic && result.NextAnthropicContentIndex > 0 {
		return result.NextAnthropicContentIndex
	}
	return len(captures)
}

func writeProviderBlockedPrompt(w io.Writer, provider conversation.Provider, result conversation.StreamingRewriteResult, text string, contentIndex int) error {
	switch provider {
	case conversation.ProviderAnthropic:
		start := map[string]any{
			"type":  "content_block_start",
			"index": contentIndex,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		}
		if err := writeSSE(w, "content_block_start", start); err != nil {
			return err
		}
		delta := map[string]any{
			"type":  "content_block_delta",
			"index": contentIndex,
			"delta": map[string]any{
				"type": "text_delta",
				"text": text,
			},
		}
		if err := writeSSE(w, "content_block_delta", delta); err != nil {
			return err
		}
		stop := map[string]any{
			"type":  "content_block_stop",
			"index": contentIndex,
		}
		if err := writeSSE(w, "content_block_stop", stop); err != nil {
			return err
		}
		return writeAnthropicStopSSE(w, "end_turn")

	case conversation.ProviderOpenAI:
		if result.StreamFormat == "openai_responses" {
			_, err := w.Write(conversation.SynthOpenAIResponsesTextSSE(text))
			return err
		}
		chunk := map[string]any{
			"id":     firstNonEmpty(result.StreamID, "chatcmpl-clawvisor"),
			"object": "chat.completion.chunk",
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"role":    "assistant",
						"content": text,
					},
				},
			},
		}
		if err := writeOpenAIData(w, chunk); err != nil {
			return err
		}
		stopChunk := map[string]any{
			"id":     firstNonEmpty(result.StreamID, "chatcmpl-clawvisor"),
			"object": "chat.completion.chunk",
			"choices": []any{
				map[string]any{
					"index":         0,
					"finish_reason": "stop",
				},
			},
		}
		if err := writeOpenAIData(w, stopChunk); err != nil {
			return err
		}
		_, err := fmt.Fprint(w, "data: [DONE]\n\n")
		return err
	}
	return nil
}

func writeProviderToolUses(w io.Writer, provider conversation.Provider, result conversation.StreamingRewriteResult, tus []conversation.ToolUse, rewrittenInput map[string]json.RawMessage) error {
	switch provider {
	case conversation.ProviderAnthropic:
		return writeAnthropicToolUsesSSE(w, tus, rewrittenInput)
	case conversation.ProviderOpenAI:
		if result.StreamFormat == "openai_responses" {
			_, err := w.Write(conversation.SynthOpenAIResponsesFunctionCallsSSE(syntheticCallsFromToolUses(tus, rewrittenInput)))
			return err
		}
		return writeOpenAIChatToolUsesSSE(w, result.StreamID, tus, rewrittenInput)
	}
	return nil
}

func writeProviderStop(w io.Writer, provider conversation.Provider, result conversation.StreamingRewriteResult) error {
	switch provider {
	case conversation.ProviderAnthropic:
		return writeAnthropicStopSSE(w, "tool_use")
	case conversation.ProviderOpenAI:
		if result.StreamFormat == "openai_responses" {
			return nil
		}
		chunk := map[string]any{
			"id":     firstNonEmpty(result.StreamID, "chatcmpl-clawvisor"),
			"object": "chat.completion.chunk",
			"choices": []any{
				map[string]any{
					"index":         0,
					"finish_reason": "tool_calls",
				},
			},
		}
		if err := writeOpenAIData(w, chunk); err != nil {
			return err
		}
		_, err := fmt.Fprint(w, "data: [DONE]\n\n")
		return err
	}
	return nil
}

func writeAnthropicToolUsesSSE(w io.Writer, tus []conversation.ToolUse, rewrittenInput map[string]json.RawMessage) error {
	for _, tu := range tus {
		input := tu.Input
		if rw, ok := rewrittenInput[tu.ID]; ok {
			input = rw
		}

		start := map[string]any{
			"type":  "content_block_start",
			"index": tu.Index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    tu.ID,
				"name":  tu.Name,
				"input": map[string]any{},
			},
		}
		if err := writeSSE(w, "content_block_start", start); err != nil {
			return err
		}

		delta := map[string]any{
			"type":  "content_block_delta",
			"index": tu.Index,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": string(input),
			},
		}
		if err := writeSSE(w, "content_block_delta", delta); err != nil {
			return err
		}

		stop := map[string]any{
			"type":  "content_block_stop",
			"index": tu.Index,
		}
		if err := writeSSE(w, "content_block_stop", stop); err != nil {
			return err
		}
	}
	return nil
}

func writeAnthropicStopSSE(w io.Writer, stopReason string) error {
	delta := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": 0},
	}
	if err := writeSSE(w, "message_delta", delta); err != nil {
		return err
	}
	return writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
}

func writeOpenAIChatToolUsesSSE(w io.Writer, streamID string, tus []conversation.ToolUse, rewrittenInput map[string]json.RawMessage) error {
	for _, tu := range tus {
		args := string(tu.Input)
		if rw, ok := rewrittenInput[tu.ID]; ok {
			args = string(rw)
		}
		chunk := map[string]any{
			"id":     firstNonEmpty(streamID, "chatcmpl-clawvisor"),
			"object": "chat.completion.chunk",
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": tu.Index,
								"id":    tu.ID,
								"type":  "function",
								"function": map[string]any{
									"name":      tu.Name,
									"arguments": args,
								},
							},
						},
					},
				},
			},
		}
		if err := writeOpenAIData(w, chunk); err != nil {
			return err
		}
	}
	return nil
}

func syntheticCallsFromToolUses(tus []conversation.ToolUse, rewrittenInput map[string]json.RawMessage) []conversation.SyntheticToolCall {
	calls := make([]conversation.SyntheticToolCall, 0, len(tus))
	for _, tu := range tus {
		input := tu.Input
		if rw, ok := rewrittenInput[tu.ID]; ok {
			input = rw
		}
		var decoded map[string]any
		if len(input) > 0 {
			_ = json.Unmarshal(input, &decoded)
		}
		if decoded == nil {
			decoded = map[string]any{}
		}
		calls = append(calls, conversation.SyntheticToolCall{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: decoded,
		})
	}
	return calls
}

func writeSSE(w io.Writer, event string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", string(raw))
	return err
}

func writeOpenAIData(w io.Writer, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", string(raw))
	return err
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
