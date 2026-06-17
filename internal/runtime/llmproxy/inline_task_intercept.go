package llmproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/controltool"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/jsonsurgery"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// inlineTaskApprovalHoldTTL bounds how long an awaiting_task_approval
// hold may sit in the cache before it's pruned. It is the decide
// window only in the sense of "we won't keep this hold around
// forever" — it does NOT cap the user's usable scope. The task's
// scope lifetime is expires_in_seconds, applied at approval time
// (see tasks_inline.go), so a generous decide window is safe:
// regardless of how long the user takes to approve, they get a full
// expires_in_seconds of usable scope starting from their "yes".
//
// 24h is chosen to comfortably cover an overnight decide gap while
// still bounding stale-approval accumulation in the cache.
const inlineTaskApprovalHoldTTL = 24 * time.Hour

// InlineTaskApprovalHoldTTL exposes the cache hold's decide window so
// callers outside llmproxy (notably the expiry sweeper in the
// approvals handler) can use the same cutoff when reaping abandoned
// chat-bound pending tasks. Lower-cased internal const remains the
// canonical value to avoid drift; the exported alias just reflects it.
const InlineTaskApprovalHoldTTL = inlineTaskApprovalHoldTTL

// InlineSurfaceQueryParam is the query-string flag the model adds to
// POST /api/control/tasks to opt in to the inline-approval flow when there
// is no prior `task` reply (e.g. the agent knows the user is sitting
// in the chat and prefers to approve there). Absent + no awaiting-
// definition hold = the existing async dashboard path.
const InlineSurfaceQueryParam = "surface"

// InlineSurfaceQueryValue is the value of the surface query parameter
// the model passes to opt in to the inline-approval flow.
const InlineSurfaceQueryValue = "inline"

// maybeInterceptInlineTaskDefinition is the postprocess hook that
// routes a model-emitted POST /api/control/tasks tool_use through the
// inline approval flow.
//
// The single opt-in signal is a `?surface=inline` query parameter on
// the URL: the agent is declaring "the user is here, approve inline."
// (An earlier state-signal path keyed on a prior
// StageAwaitingTaskDefinition hold was removed once
// RewriteTaskApprovalReply switched to fully Resolving the original
// tool hold on "task" reply — no awaiting-definition hold ever exists
// in production traffic for the intercept to observe.)
//
// When the query signal fires, the model never actually POSTs the
// task — the tool_use_result is replaced with a rendered yes/no
// prompt, and the user's next "yes" creates the task pre-approved.
//
// Returns (_, false) when the signal is absent, the body fails to
// parse, or the path isn't POST /api/control/tasks — callers should
// fall through to the regular control-rewrite path so headless task
// creation still routes through the dashboard handler unchanged.
// MaybeInterceptInlineTaskDefinition is the entry point for inline
// task-definition interception. Used by the handler-side pipeline
// factory's ControlToolUseEvaluator InterceptInline hook. Audit +
// trace callbacks are supplied per-call so the caller routes events
// into whichever sink it owns.
func MaybeInterceptInlineTaskDefinition(
	req *http.Request,
	cfg PostprocessConfig,
	audit func(decision, outcome, reason string),
	trace func(event string, kv ...any),
	provider conversation.Provider,
	tu conversation.ToolUse,
	call ControlCall,
) (conversation.ToolUseVerdict, bool) {
	if cfg.PendingApprovals == nil {
		return conversation.ToolUseVerdict{}, false
	}
	// Only intercept POSTs to /api/control/tasks; the dashboard handler
	// covers GETs (skill catalog) and other control paths. Exact
	// path equality — HasSuffix would also match attacker-shaped paths
	// like /foo/bar/api/control/tasks if the host check ever loosened.
	if !strings.EqualFold(call.Method, "POST") || call.URL.Path != "/api/control/tasks" {
		return conversation.ToolUseVerdict{}, false
	}

	// Query signal: agent explicitly opted in via ?surface=inline. This
	// is the only signal we honor in production — the older "state
	// signal" branch (a prior StageAwaitingTaskDefinition hold from a
	// "task" reply) is unreachable now that RewriteTaskApprovalReply
	// fully Resolves the original tool hold rather than transitioning
	// its stage. taskCreationPrompt teaches the model to include
	// ?surface=inline, so compliant traffic flows through here; the
	// query-less path correctly falls through to the dashboard rewrite.
	// Both key and value match case-SENSITIVELY: `url.Values.Get` is
	// case-sensitive on the key, and harnesses emit the exact
	// surface=inline string we teach them in taskCreationPrompt.
	// Mixed-case (Surface=INLINE) is not a shape we promise to honor;
	// keeping symmetric strictness avoids future-reader surprise.
	querySignal := call.URL.Query().Get(InlineSurfaceQueryParam) == InlineSurfaceQueryValue
	if !querySignal {
		return conversation.ToolUseVerdict{}, false
	}

	// On the failure paths below, we audit with decision="fallthrough"
	// rather than "block" because the function returns (verdict{}, false)
	// and the caller proceeds to the regular control-rewrite path.
	// Emitting "block" here would record a misleading audit row paired
	// with whatever decision the dashboard rewriter ultimately reaches
	// for the same tool_use — operators chasing inline-task failures
	// would see a "block" followed by an unrelated outcome for the
	// same request.
	bodyBytes, ok := controlTaskBodyFromInput(tu.Input)
	if !ok || len(bodyBytes) == 0 {
		audit("fallthrough", "inline_task_body_missing", "POST /api/control/tasks had no body; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}
	parsed := &runtimetasks.TaskCreateRequest{}
	if err := json.Unmarshal(bodyBytes, parsed); err != nil {
		audit("fallthrough", "inline_task_body_malformed", err.Error())
		return conversation.ToolUseVerdict{}, false
	}
	if strings.TrimSpace(parsed.Purpose) == "" {
		audit("fallthrough", "inline_task_missing_purpose", "task body missing purpose; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}
	env := runtimetasks.Envelope{
		ExpectedTools:          parsed.ExpectedTools,
		ExpectedEgress:         parsed.ExpectedEgress,
		RequiredCredentials:    parsed.RequiredCredentials,
		IntentVerificationMode: parsed.IntentVerificationMode,
		ExpectedUse:            parsed.ExpectedUse,
		SchemaVersion:          parsed.SchemaVersion,
	}
	if env.SchemaVersion == 0 {
		env.SchemaVersion = 2
	}
	if env.IntentVerificationMode == "" {
		env.IntentVerificationMode = "strict"
	}
	if issues := runtimepolicy.ValidateTaskEnvelope(env); len(issues) > 0 {
		audit("fallthrough", "inline_task_invalid_envelope", inlineTaskValidationReason(issues)+"; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}

	// Lifetime/expires conflict: standing tasks reject expires_in_seconds
	// at creation time (see tasks_inline.go's createInlineApprovedTask).
	// Catch the same conflict here before rendering — otherwise the user
	// would see a "Lifetime: always" prompt, approve, and then watch the
	// release path fail with a confusing error. Fall through to the
	// dashboard rewrite so the model gets the same JSON error it would
	// receive from a direct POST, keeping behavior consistent across
	// surfaces.
	if strings.EqualFold(strings.TrimSpace(parsed.Lifetime), "standing") && parsed.ExpiresInSeconds > 0 {
		audit("fallthrough", "inline_task_invalid_envelope", "expires_in_seconds cannot be set on a standing task; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}

	// Risk assessment runs BEFORE the hold so the auto-approval gate
	// can decide whether to skip the human prompt entirely. The
	// assessment is also used to render the prompt on the fall-through
	// path, so we compute it once.
	assessment := assessInlineTaskRisk(req, cfg, parsed, env, trace)

	// Conversation-based auto-approval. If the user's recent turns
	// authorize the requested scope and the risk level is at-or-below
	// the user's configured threshold (with no conflicts), create the
	// task pre-approved and substitute the success augmentation —
	// no human prompt, no hold.
	ok, reason, refusal := autoApproveFromConversation(cfg, assessment)
	if !ok {
		// Trace why the gate refused so operators chasing "auto-approval
		// didn't fire" have a deterministic answer in the log instead of
		// having to guess from prompt-vs-no-prompt behavior. Recorded
		// even when the threshold is "off" — that's the most common
		// "miss" and operators should still see the agent's actual
		// configuration in the log.
		intentMatch := ""
		riskLevel := ""
		if assessment != nil {
			intentMatch = assessment.IntentMatch
			riskLevel = assessment.RiskLevel
		}
		trace("inline_task.auto_approve_refused",
			"refusal", refusal,
			"threshold", cfg.ConversationAutoApproveThreshold,
			"risk_level", riskLevel,
			"intent_match", intentMatch,
			"recent_user_turns", len(cfg.RecentUserTurns),
		)
	}
	if ok {
		if cfg.InlineTaskCreator == nil {
			// Threshold says "approve" but the runtime cannot create
			// the task without prompting (no creator wired). Fall
			// through to the human approval path; logged as a
			// configuration gap for operators.
			audit("fallthrough", "auto_approve_creator_missing", "conversation gate covered but no inline task creator configured")
			trace("inline_task.auto_approve_creator_missing", "threshold", cfg.ConversationAutoApproveThreshold)
		} else {
			// Include Name so the handler-side risk assessor's
			// AgentName field matches the manual approval path (which
			// receives the middleware-loaded agent).
			//
			// Fast-path the precomputed assessment when the
			// implementation honors it (TasksHandler does). Avoids a
			// second LLM round-trip AND keeps the persisted
			// task.RiskLevel byte-identical to the level that
			// justified bypassing the prompt — otherwise the assessor
			// can return a different verdict on the second call and
			// dashboards show a level the gate didn't actually accept.
			agentForCreate := &store.Agent{ID: cfg.AgentID, UserID: cfg.AgentUserID, Name: cfg.AgentName}
			var created *InlineApprovedTask
			var createErr error
			if withAssessment, ok := cfg.InlineTaskCreator.(InlineTaskCreatorWithAssessment); ok {
				created, createErr = withAssessment.CreateInlineApprovedTaskWithAssessment(req.Context(), agentForCreate, parsed, tu.ID, assessment)
			} else {
				created, createErr = cfg.InlineTaskCreator.CreateInlineApprovedTask(req.Context(), agentForCreate, parsed, tu.ID)
			}
			if createErr != nil {
				// Create failed — log and fall through to the prompt
				// path so the user can still approve manually.
				audit("fallthrough", "auto_approve_create_failed", createErr.Error())
				trace("inline_task.auto_approve_create_failed", "err", createErr.Error())
			} else {
				checkedOut := false
				if cfg.Checkouts != nil && created.ID != "" {
					// Include ConversationID for parity with the manual
					// approval path (inline_task_transitions.go's Set call).
					// Without it the entry lands in the legacy (user, agent)
					// bucket which is the cross-conversation fallback —
					// approving a task in conversation A would silently
					// become conversation B's preferred task on its next
					// call, contradicting the TaskCheckoutKey contract.
					if setErr := cfg.Checkouts.Set(req.Context(), TaskCheckoutKey{
						UserID:         cfg.AgentUserID,
						AgentID:        cfg.AgentID,
						ConversationID: cfg.ConversationID,
					}, created.ID, 0); setErr == nil {
						checkedOut = true
					}
				}
				audit("approve", "auto_approved_from_conversation", reason)
				// Task-linked audit row. The generic tool_use row above
				// records that the intercept fired; this one records
				// WHICH task got auto-approved so downstream consumers
				// filtering by task_id can reconstruct the chain —
				// matching the manual-approval path's
				// LogInlineTaskApproved behavior, with a distinct event
				// name so dashboards can distinguish gate-bypassed
				// approvals from human ones.
				if cfg.Audit != nil {
					if auditAgent := AuditAgentForCfg(cfg); auditAgent != nil {
						cfg.Audit.LogInlineTaskAutoApproved(
							req.Context(),
							auditAgent,
							cfg.RequestID,
							tu.ID,
							created,
							reason,
							assessment.RiskLevel,
							assessment.IntentMatch,
							cfg.ConversationAutoApproveThreshold,
						)
					}
				}
				trace("inline_task.auto_approved",
					"task_id", created.ID,
					"risk_level", assessment.RiskLevel,
					"intent_match", assessment.IntentMatch,
					"threshold", cfg.ConversationAutoApproveThreshold,
					"checked_out", checkedOut,
					"reason", reason,
				)
				augmentation := inlineApprovedReplyAugmentationContext(created.ID, checkedOut, created.Credentials)
				continuationPayload, _ := jsonsurgery.MarshalNoEscape(augmentation)
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: auto-approved from conversation context",
					// CreatedTaskID lets downstream audit emissions
					// (LogContinuationSkippedSiblingTools etc.) link
					// to the same task without parsing the
					// augmentation text or threading a sidecar map.
					CreatedTaskID: created.ID,
					// SubstituteWith is the fallback rendered to the
					// harness as an assistant text turn if the handler
					// can't complete the recursive continuation call
					// (unsupported provider, recursion bound reached,
					// upstream error). Continue is the happy path: the
					// handler feeds this same text back upstream as a
					// synthetic user/tool_result turn so the model can
					// proceed without bouncing to the user.
					SubstituteWith: augmentation,
					Continue: &conversation.ContinueSignal{
						SyntheticToolResults: []json.RawMessage{continuationPayload},
						PrependNotice:        AutoApproveUserNotice(created.Purpose),
					},
				}, true
			}
		}
	}

	// Create the pending Task row BEFORE the cache hold so the
	// dashboard's Tasks page renders this work as a pending task while
	// the cache hold awaits the user's reply (status='pending_approval',
	// approval_source='inline_chat'). The dashboard's Approve / Deny
	// endpoints refuse to act on inline_chat-bound pending rows; the
	// cache hold is the in-process resolution path.
	//
	// When the configured creator doesn't implement the pending-flow
	// extension (legacy test stubs, runtimes wired before this
	// change), skip the pending-task landing and fall back to the
	// legacy create-on-approve flow — the prompt still renders and
	// the chat path still resolves via resolved.TaskDefinition. The
	// only loss is the dashboard's pending-task surface for those
	// callers, which they didn't have before.
	pendingCreator, _ := cfg.InlineTaskCreator.(InlineTaskPendingCreator)
	var pendingTaskID string
	if pendingCreator != nil {
		agentForCreate := &store.Agent{ID: cfg.AgentID, UserID: cfg.AgentUserID, Name: cfg.AgentName}
		created, pendingErr := pendingCreator.CreatePendingInlineTask(req.Context(), agentForCreate, parsed, tu.ID, assessment)
		if pendingErr != nil {
			// fallthrough (not block) — we return false, so the
			// caller proceeds to the regular control-rewrite path.
			// See the lines 99-106 comment for the rationale.
			audit("fallthrough", "inline_task_pending_create_failed", pendingErr.Error()+"; deferring to dashboard rewrite")
			trace("inline_task.pending_create_failed", "err", pendingErr.Error())
			return conversation.ToolUseVerdict{}, false
		}
		pendingTaskID = created
	}

	now := time.Now().UTC()
	innerHold, holdErr := cfg.PendingApprovals.Hold(req.Context(), PendingLiteApproval{
		UserID:          cfg.AgentUserID,
		AgentID:         cfg.AgentID,
		Provider:        provider,
		ConversationID:  cfg.ConversationID,
		ToolUse:         tu,
		Reason:          "inline task creation awaiting user approval",
		Stage:           StageAwaitingTaskApproval,
		TaskDefinition:  parsed,
		PrecomputedRisk: assessment,
		PendingTaskID:   pendingTaskID,
		ScopeDriftID:    parsed.DriftID,
		CreatedAt:       now,
		ExpiresAt:       now.Add(inlineTaskApprovalHoldTTL),
	})
	if holdErr != nil {
		// Cache hold failed. If we already landed a pending task in
		// the DB, roll it back so the dashboard doesn't show an
		// orphaned pending task whose cache anchor never existed.
		// Use a detached context so a mid-request client disconnect
		// (a plausible cause of cache misbehavior) doesn't cancel
		// the rollback and strand the orphan for the full 24h TTL.
		// Cap it at 5s so a stalled store backend can't block the
		// inbound request goroutine indefinitely — WithoutCancel
		// alone strips the parent deadline too.
		if pendingCreator != nil && pendingTaskID != "" {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 5*time.Second)
			expireErr := pendingCreator.ExpireInlineTask(rollbackCtx, pendingTaskID, cfg.AgentUserID)
			cancel()
			if expireErr != nil {
				trace("inline_task.pending_rollback_failed", "task_id", pendingTaskID, "err", expireErr.Error())
			}
		}
		// fallthrough — see the audit-decision rationale above.
		audit("fallthrough", "inline_task_hold_failed", holdErr.Error()+"; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}
	if innerHold.Evicted != nil {
		// This direct Hold path can displace an older inline-task
		// approval when the bounded cache is full. Expire the evicted
		// task's DB anchor so the dashboard doesn't keep showing
		// "reply in chat" guidance for a hold that can no longer be
		// resolved from chat.
		CleanupEvictedInlineTask(req.Context(), cfg, innerHold.Evicted)
	}

	audit("approve", "pending", "inline_task_pending_approval: awaiting user yes/no on inline task definition (query)")
	trace("inline_task.held",
		"approval_id", innerHold.Pending.ID,
		"task_id", pendingTaskID,
		"purpose", parsed.Purpose,
		"signal", "query",
	)
	// Audit the in-flight task creation into task_lifecycle_events.
	// Same rationale as the expansion-side write: the body editor on
	// the next turn falls back to this row when the in-memory
	// outcome cache misses (proxy restart between hold and approval).
	// Best-effort; failures land in the trace channel and do NOT
	// strand the hold.
	if pendingTaskID != "" {
		if payload, marshalErr := json.Marshal(parsed); marshalErr == nil {
			logTaskLifecycleEventFromHold(req.Context(), taskLifecycleAuditCtx{
				st:             cfg.Store,
				trace:          trace,
				userID:         cfg.AgentUserID,
				agentID:        cfg.AgentID,
				conversationID: cfg.ConversationID,
				requestID:      cfg.RequestID,
			}, pendingTaskID, innerHold.Pending.ID, store.TaskLifecycleEventTaskCreatePending, "inline_chat", tu, payload)
		}
	}
	promptText := renderTaskApprovalPromptWithRiskAndTools(parsed, innerHold.Pending.ID, assessment, cfg.DefaultTaskExpirySeconds, cfg.AvailableTools)
	verdict := conversation.ToolUseVerdict{
		Allowed:        false,
		Reason:         "Clawvisor: awaiting inline task approval",
		SubstituteWith: promptText,
		HeldKindHint:   "approval",
	}
	useAskUserQuestion := askUserQuestionAvailable(cfg.AvailableTools)
	trace("inline_task.substitution_shape",
		"approval_id", innerHold.Pending.ID,
		"shape", inlineSubstitutionShape(useAskUserQuestion),
		"available_tool_count", len(cfg.AvailableTools),
		"ask_user_question_present", useAskUserQuestion,
		"available_tools", cfg.AvailableTools,
	)
	if useAskUserQuestion {
		// Emit a text block (the rendered prompt with the approval
		// marker — via SubstituteWith) PLUS a minimal
		// AskUserQuestion(yes/no) tool_use call. The codec writes
		// the text block first so the harness surfaces the task
		// definition in chat, then the picker pops up with just
		// "Approve this task?" — the task body doesn't get
		// duplicated inside the picker. The parser scans the same
		// assistant turn's text content for the [clawvisor:approval=...]
		// marker, so correlation still works without burying the
		// marker in the picker question.
		verdict.SubstituteWithToolCall = buildAskUserQuestionToolCall(innerHold.Pending.ID)
	}
	return verdict, true
}

func inlineSubstitutionShape(useAskUserQuestion bool) string {
	if useAskUserQuestion {
		return "ask_user_question"
	}
	return "text"
}

// buildAskUserQuestionToolCall constructs the synthetic
// AskUserQuestion tool_use for the task-creation surface. Wraps the
// generic builder with the task-creation question/header strings.
func buildAskUserQuestionToolCall(approvalID string) *conversation.SyntheticToolCall {
	return buildAskUserQuestionApprovalCall(approvalID, askUserQuestionApprovalSpec{
		Question:       "Approve this task?",
		Header:         "Approve task",
		YesDescription: "Authorize the task",
	})
}

// askUserQuestionApprovalSpec captures the parts of the
// AskUserQuestion picker that change between surfaces (task
// creation, scope expansion). Keep it small — Header is the
// click-tag in the UI and tight on chars, Question is the
// long-form ask.
type askUserQuestionApprovalSpec struct {
	Question       string
	Header         string
	YesDescription string
}

// buildAskUserQuestionApprovalCall constructs the synthetic
// AskUserQuestion tool_use the harness sees when AskUserQuestion is
// in the agent's declared tool list. The picker body is intentionally
// minimal — the actual approval context (task body or expansion
// delta) lives in a sibling text block emitted alongside this
// tool_use, so duplicating it inside the picker would just clutter
// the UI.
//
// The [clawvisor:approval=cv-...] marker is NOT embedded in the
// question; it lives in the sibling text block and is found there
// by the parser. The synthetic tool_use_id still namespaces under
// the approval ID so audit logs and trace records correlate the
// AskUserQuestion call back to the hold without an extra lookup.
//
// SyntheticToolUseIDPrefix is the same namespace historystrip uses
// to identify orphaned synthetic tool_uses on subsequent turns —
// producer / consumer stay in lockstep via this constant.
func buildAskUserQuestionApprovalCall(approvalID string, spec askUserQuestionApprovalSpec) *conversation.SyntheticToolCall {
	id := SyntheticToolUseIDPrefix + "ask"
	if approvalID != "" {
		id = SyntheticToolUseIDPrefix + "ask_" + approvalID
	}
	return &conversation.SyntheticToolCall{
		ID:   id,
		Name: AskUserQuestionToolName,
		Input: map[string]any{
			"questions": []map[string]any{
				{
					"question":    spec.Question,
					"header":      spec.Header,
					"multiSelect": false,
					"options": []map[string]any{
						{"label": "Yes", "description": spec.YesDescription},
						{"label": "No", "description": "Cancel"},
					},
				},
			},
		},
	}
}

// CleanupEvictedInlineTask expires the store.Task row anchoring an
// evicted inline-task hold. The LRU cache only carries N holds per
// (user, agent, provider, conversation) tuple; when a new Hold
// displaces an older inline-task hold, the cache anchor is gone and
// chat approve can never resolve the row. Without this the dashboard
// would keep showing the row as pending_approval with a "reply in
// chat" notice that can never succeed — exactly the zombie shape
// the dashboard-deny escape hatch was meant to solve, but
// automatically, since the user has no signal that the cache evicted
// anything.
//
// No-op on holds without a PendingTaskID (non-inline holds, or
// inline holds minted before the pending-task surface was wired),
// or when the creator doesn't implement the pending extension.
// Safe to call unconditionally on any eviction.
func CleanupEvictedInlineTask(ctx context.Context, cfg PostprocessConfig, evicted *PendingLiteApproval) {
	if evicted == nil || evicted.PendingTaskID == "" || evicted.UserID == "" {
		return
	}
	pendingCreator, ok := cfg.InlineTaskCreator.(InlineTaskPendingCreator)
	if !ok || pendingCreator == nil {
		return
	}
	// Bounded detached context so a stalled store backend can't
	// hang the inbound request goroutine, and a mid-request client
	// disconnect doesn't strand the row at pending.
	expireCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := pendingCreator.ExpireInlineTask(expireCtx, evicted.PendingTaskID, evicted.UserID); err != nil && cfg.Trace != nil {
		cfg.Trace.Emit(map[string]any{
			"event":       "inline_task.evicted_expire_failed",
			"request_id":  cfg.RequestID,
			"user_id":     evicted.UserID,
			"agent_id":    evicted.AgentID,
			"approval_id": evicted.ID,
			"task_id":     evicted.PendingTaskID,
			"err":         err.Error(),
		})
	}
}

// hasNonEmptyTurn reports whether the slice contains at least one
// turn with non-whitespace content. Used by the auto-approve gate as
// the deterministic "did the user actually say anything?" check so
// the floor isn't trivially defeated by a future caller passing in
// a slice of whitespace strings.
func hasNonEmptyTurn(turns []string) bool {
	for _, t := range turns {
		if strings.TrimSpace(t) != "" {
			return true
		}
	}
	return false
}

// AutoApproveUserNotice renders the human-facing one-liner the
// handler prepends to the continuation's assistant turn after the
// gate fires. Quoting the task purpose makes the message
// self-describing — the user sees both that an auto-approval
// happened AND what was approved, without needing to look at the
// dashboard. The purpose is model-authored, so we strip control
// characters and cap the length defensively so a runaway purpose
// can't dominate the assistant turn.
func AutoApproveUserNotice(purpose string) string {
	const maxPurposeRunes = 200
	cleaned := strings.TrimSpace(purpose)
	cleaned = strings.ReplaceAll(cleaned, "\r", " ")
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	// Backticks would terminate the markdown inline-code span the
	// notice is wrapped in and leak the remainder as prose. The
	// purpose is model-authored, so strip them defensively.
	cleaned = strings.ReplaceAll(cleaned, "`", "")
	// Truncate by rune count, not bytes. Slicing a multibyte UTF-8
	// string at an arbitrary byte index splits a rune mid-sequence
	// and the resulting invalid UTF-8 renders as U+FFFD once JSON-
	// marshalled. The purpose is model-controlled so a non-ASCII
	// purpose (Chinese, emoji, accented Latin) over 200 bytes would
	// deterministically produce a garbled notice without this.
	if utf8.RuneCountInString(cleaned) > maxPurposeRunes {
		// Walk runes and stop at the cap so we slice on a rune
		// boundary. strings.TrimSpace + ReplaceAll above don't break
		// rune boundaries, so cleaned is valid UTF-8 here.
		runeCount := 0
		cutByte := len(cleaned)
		for i := range cleaned {
			if runeCount == maxPurposeRunes {
				cutByte = i
				break
			}
			runeCount++
		}
		cleaned = cleaned[:cutByte] + "…"
	}
	// Backticked `[Clawvisor]` form: lands in the assistant turn, so
	// it's user-facing — the human reading the chat needs a clear,
	// code-styled status line. The `<clawvisor-notice>` tag is reserved
	// for user-role injections the LLM reads.
	if cleaned == "" {
		return "`[Clawvisor] Task auto-approved based on your recent request.`"
	}
	return "`[Clawvisor] Task auto-approved: " + cleaned + "`"
}

// autoApproveFromConversation reports whether the conversation-based
// auto-approval gate should fire for the given assessment + config.
// Four independent conditions must all hold:
//
//  1. At least one trusted recent user turn was extracted by the
//     deterministic walker. This is the security floor: the gate
//     refuses to fire on the LLM's intent verdict alone, because a
//     misbehaving or compromised assessor could otherwise return
//     intent_match="yes" despite having seen no human input at all.
//     Requiring non-empty turns here means the runtime — not the LLM
//     — owns the "did the user actually say anything?" question.
//  2. The user has opted in by setting a non-"off" threshold.
//  3. The assessor returned intent_match="yes" — the user's recent
//     turns plainly authorize the requested scope. "partial" / "no" /
//     "unknown" fall through to the human prompt; ambiguity is the
//     user's call, not ours.
//  4. The risk level is at-or-below the user's threshold and the
//     assessor reported no internal conflicts. A conflicting task
//     (purpose vs. expected_use mismatch, etc.) always reaches the
//     human regardless of intent_match, because the conflict is
//     evidence the agent's task envelope isn't what the user thinks
//     they're approving.
//
// Returns (true, audit_reason, "") when all four hold;
// (false, "", refusal_reason) otherwise. refusal_reason is a short
// machine-readable string ("no_recent_turns", "threshold_off",
// "risk_above_threshold", "intent_match_not_yes",
// "intent_match_conflicts", "no_assessment") so operators chasing a
// missing auto-approval can see exactly which gate refused without
// inferring it from prompt-vs-no-prompt observation.
func autoApproveFromConversation(cfg PostprocessConfig, assessment *taskrisk.RiskAssessment) (bool, string, string) {
	// assessInlineTaskRisk always returns at least the deterministic
	// envelope assessment (never nil), so a nil assessment here is
	// unreachable in production. Defensive zero-value handling keeps
	// the function total in case a test or future caller wires the
	// gate with a stubbed/nil assessor; collapses into the same
	// audit-trail outcome as "no risk level emitted at all," which
	// is the safest fail-closed read.
	if assessment == nil {
		return false, "", "no_assessment"
	}
	// Deterministic floor: no extracted human turns ⇒ no auto-approval,
	// no matter what the assessor claims. ExtractRecentHumanTurns
	// already trims whitespace and filters Clawvisor-internal verbs,
	// but we re-check content here so the floor holds even if a
	// future caller routes raw turns into cfg without going through
	// the extractor — the gate owns "did the user actually say
	// anything?" and shouldn't delegate that to an upstream invariant.
	if !hasNonEmptyTurn(cfg.RecentUserTurns) {
		return false, "", "no_recent_turns"
	}
	if strings.EqualFold(strings.TrimSpace(cfg.ConversationAutoApproveThreshold), "off") ||
		strings.TrimSpace(cfg.ConversationAutoApproveThreshold) == "" {
		return false, "", "threshold_off"
	}
	if !store.ConversationAutoApproveCovers(cfg.ConversationAutoApproveThreshold, assessment.RiskLevel) {
		return false, "", "risk_above_threshold"
	}
	if !strings.EqualFold(strings.TrimSpace(assessment.IntentMatch), "yes") {
		return false, "", "intent_match_not_yes"
	}
	if len(assessment.Conflicts) > 0 {
		return false, "", "intent_match_conflicts"
	}
	return true, "risk=" + assessment.RiskLevel + ", intent_match=yes, threshold=" + cfg.ConversationAutoApproveThreshold, ""
}

// assessInlineTaskRisk returns the LLM-backed risk assessor's verdict when
// it is configured and produces a usable answer; otherwise it falls back to
// the deterministic envelope-shape policy. The envelope policy is only the
// fallback path now — when the LLM verdict comes back, it is taken as the
// truth and the deterministic read is discarded.
func assessInlineTaskRisk(
	req *http.Request,
	cfg PostprocessConfig,
	parsed *runtimetasks.TaskCreateRequest,
	env runtimetasks.Envelope,
	trace func(event string, kv ...any),
) *taskrisk.RiskAssessment {
	if cfg.TaskRiskAssessor == nil {
		return runtimepolicy.AssessTaskEnvelope(parsed.Purpose, env)
	}

	llmVerdict := cfg.TaskRiskAssessor.AssessEnvelope(req.Context(), TaskRiskAssessRequest{
		Purpose:                parsed.Purpose,
		AgentName:              cfg.AgentName,
		UserID:                 cfg.AgentUserID,
		ExpectedTools:          env.ExpectedTools,
		ExpectedEgress:         env.ExpectedEgress,
		RequiredCredentials:    env.RequiredCredentials,
		IntentVerificationMode: env.IntentVerificationMode,
		ExpectedUse:            env.ExpectedUse,
		RecentUserTurns:        cfg.RecentUserTurns,
	})
	if llmVerdict == nil || strings.EqualFold(strings.TrimSpace(llmVerdict.RiskLevel), "unknown") {
		trace("inline_task.risk_llm_unavailable")
		return runtimepolicy.AssessTaskEnvelope(parsed.Purpose, env)
	}

	conflicts := make([]taskrisk.ConflictDetail, 0, len(llmVerdict.Conflicts))
	for _, c := range llmVerdict.Conflicts {
		conflicts = append(conflicts, taskrisk.ConflictDetail{
			Field:       c.Field,
			Description: c.Description,
			Severity:    c.Severity,
		})
	}
	return &taskrisk.RiskAssessment{
		RiskLevel:              llmVerdict.RiskLevel,
		Explanation:            llmVerdict.Explanation,
		Factors:                llmVerdict.Factors,
		Conflicts:              conflicts,
		IntentMatch:            llmVerdict.IntentMatch,
		IntentMatchExplanation: llmVerdict.IntentMatchExplanation,
	}
}

func inlineTaskValidationReason(issues []runtimepolicy.ValidationIssue) string {
	var parts []string
	for _, issue := range issues {
		parts = append(parts, issue.Field+": "+issue.Message)
	}
	return strings.Join(parts, "; ")
}

// controlTaskBodyFromInput extracts the POST body from the tool_use's
// structured or command form. Mirrors ParseControlToolUseWithBase's
// reachable shapes but returns just the body bytes — the URL has
// already been classified by the caller. Routes through the shared
// parseControlCmd helper so both single-statement (curl with stdin
// heredoc) and multi-statement (cat-heredoc + curl --data @file)
// shapes resolve to the actual body bytes.
func controlTaskBodyFromInput(in json.RawMessage) ([]byte, bool) {
	return controltool.TaskBodyFromInput(in)
}
