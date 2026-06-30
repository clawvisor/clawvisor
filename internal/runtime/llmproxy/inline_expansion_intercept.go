package llmproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// expandPathSuffix is the trailing path segment on the synthetic
// control URL the intercept claims. We match the FULL path
// "/api/control/tasks/{id}/expand" by recognizing the prefix and
// trailing suffix together — HasPrefix + HasSuffix is robust to the
// id segment without requiring a regex compile per tool_use.
const (
	expandPathPrefix = "/api/control/tasks/"
	expandPathSuffix = "/expand"
)

// inlineExpandRequestBody is the shape the model posts to the expand
// URL. Mirrors expandTaskRequest in the handler but lives here so the
// llmproxy package doesn't import the handlers package (which would
// cycle). Field tags match the wire format.
type inlineExpandRequestBody struct {
	ExpectedTools       []runtimetasks.ExpectedTool       `json:"expected_tools,omitempty"`
	ExpectedEgress      []runtimetasks.ExpectedEgress     `json:"expected_egress,omitempty"`
	RequiredCredentials []runtimetasks.RequiredCredential `json:"required_credentials,omitempty"`
	Reason              string                            `json:"reason"`
	// DriftID, when non-empty, links this expand to a scope-drift menu
	// the agent just resolved. On user approval the resolver calls
	// ScopeDriftRegistry.SetOutcome so the agent's retry of the
	// originally-blocked tool_use consumes the one-shot pre-clear.
	DriftID string `json:"drift_id,omitempty"`
}

// MaybeInterceptInlineExpansion is the postprocess hook that routes a
// model-emitted POST /api/control/tasks/{id}/expand?surface=inline
// tool_use through the inline approval flow. Mirror of
// MaybeInterceptInlineTaskDefinition for the expansion path: when
// the query signal fires, the model never actually POSTs the expand
// — the tool_use_result is replaced with a rendered yes/no prompt,
// and the user's next "yes" approves the expansion against the
// already-landed pending state.
//
// Returns (_, false) when the signal is absent, the body fails to
// parse, or the path/method doesn't match — callers should fall
// through to the regular control-rewrite path so headless expansion
// still routes through the dashboard handler unchanged.
func MaybeInterceptInlineExpansion(
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
	// Match POST /api/control/tasks/{id}/expand exactly. HasPrefix +
	// HasSuffix is the simplest robust shape — the {id} segment is
	// agent-supplied and we want exact-suffix to refuse attacker
	// paths like /api/control/tasks/x/expand/y.
	if !strings.EqualFold(call.Method, "POST") {
		return conversation.ToolUseVerdict{}, false
	}
	path := call.URL.Path
	if !strings.HasPrefix(path, expandPathPrefix) || !strings.HasSuffix(path, expandPathSuffix) {
		return conversation.ToolUseVerdict{}, false
	}
	// Extract the {id} segment. The middle must be a single non-empty
	// segment (no further /). This rejects /api/control/tasks/x/y/expand
	// at the boundary.
	mid := strings.TrimSuffix(strings.TrimPrefix(path, expandPathPrefix), expandPathSuffix)
	if mid == "" || strings.Contains(mid, "/") {
		return conversation.ToolUseVerdict{}, false
	}
	taskID := mid

	// Opt-in signal: same `?surface=inline` as task creation. Compliant
	// models that don't add the flag fall through to the headless
	// expand handler (which sends dashboard / Telegram / push prompts).
	querySignal := call.URL.Query().Get(InlineSurfaceQueryParam) == InlineSurfaceQueryValue
	if !querySignal {
		return conversation.ToolUseVerdict{}, false
	}

	// Look up the parent task FIRST so we can refuse early on a
	// missing / wrong-owner row, before any side effects. The same
	// store-lookup happens inside CreatePendingInlineExpansion (via
	// the existing Expand validation), but doing it here lets us
	// reject + audit with a clean reason at intercept time. Also
	// gives us the parent's purpose for the rendered prompt.
	if cfg.InlineTaskCreator == nil {
		audit("fallthrough", "inline_expansion_creator_missing", "no inline-task creator configured on this daemon; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}
	expansionCreator, ok := cfg.InlineTaskCreator.(InlineExpansionCreator)
	if !ok {
		audit("fallthrough", "inline_expansion_creator_unsupported", "creator does not implement InlineExpansionCreator; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}

	bodyBytes, ok := controlTaskBodyFromInput(tu.Input)
	if !ok || len(bodyBytes) == 0 {
		audit("fallthrough", "inline_expansion_body_missing", "POST .../expand had no body; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}
	parsed := inlineExpandRequestBody{}
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		audit("fallthrough", "inline_expansion_body_malformed", err.Error())
		return conversation.ToolUseVerdict{}, false
	}
	if strings.TrimSpace(parsed.Reason) == "" {
		audit("fallthrough", "inline_expansion_missing_reason", "expand body missing reason; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}
	additions := runtimetasks.Envelope{
		ExpectedTools:       parsed.ExpectedTools,
		ExpectedEgress:      parsed.ExpectedEgress,
		RequiredCredentials: parsed.RequiredCredentials,
	}
	if len(additions.ExpectedTools) == 0 && len(additions.ExpectedEgress) == 0 && len(additions.RequiredCredentials) == 0 {
		audit("fallthrough", "inline_expansion_empty_additions", "expand body has no additions; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}
	if issues := runtimepolicy.ValidateTaskEnvelopeAdditions(additions); len(issues) > 0 {
		audit("fallthrough", "inline_expansion_invalid_envelope", inlineTaskValidationReason(issues)+"; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}

	guard := NewDriftClaimGuard(req.Context(), cfg.ScopeDrifts, parsed.DriftID)
	defer guard.Rollback()

	if parsed.DriftID != "" {
		// Mirror the task-creation intercept: an unclaimable drift_id
		// must not demote the surface. Treat as no drift_id and continue
		// inline. See MaybeInterceptInlineTaskDefinition for the full
		// rationale.
		guardAudit := func(decision, outcome, reason string) {
			if decision == "fallthrough" {
				decision = "continue"
			}
			audit(decision, outcome, reason)
		}
		if _, claimedOk := guard.Claim(
			cfg.AgentID,
			cfg.ConversationID,
			ScopeDriftOptionExpand,
			"",
			guardAudit,
		); !claimedOk {
			// Wipe so the cache hold (line ~205 sets ScopeDriftID) and
			// the downstream verdict don't carry an id we never claimed.
			parsed.DriftID = ""
		}
	}

	// Fetch the parent task FIRST so we can derive the merged envelope
	// and compute the LLM+floor risk BEFORE landing the pending row.
	// We want the assessment to flow into both: (a) the pending row's
	// cached RiskAssessment, so a dashboard-side approve also reuses
	// it; and (b) the in-memory hold (below), so the chat-side approve
	// can short-circuit reassessExpansionRisk's LLM call. Best-effort
	// store lookup — on failure the prompt renders with what we have
	// and the approve path falls back to deterministic-only.
	parentPurpose := ""
	parentLifetime := ""
	var expansionRisk *taskrisk.RiskAssessment
	if cfg.Store != nil {
		if parent, err := cfg.Store.GetTask(req.Context(), taskID); err == nil && parent != nil {
			parentPurpose = parent.Purpose
			parentLifetime = parent.Lifetime
			if parentEnv, envErr := runtimetasks.EnvelopeFromTask(parent); envErr == nil {
				merge := runtimetasks.MergeEnvelopes(parentEnv, additions)
				expansionRisk = assessInlineExpansionRisk(req, cfg, parent.Purpose, merge.Merged, trace)
			}
		}
	}

	// Land the pending state in the DB before holding so the dashboard
	// sees the in-flight expansion as a pending row even while the
	// chat anchor owns the decide window. The creator runs the same
	// derived-action + credential gates the public Expand handler
	// uses, so any failure path here is identical in shape to the
	// headless deny — same error text the agent would have gotten.
	// The precomputed assessment is passed through so the pending row
	// carries the same cached RiskAssessment the headless Expand
	// handler stamps — that way a dashboard-side approve (race) also
	// reuses the LLM verdict.
	agentForCreate := &store.Agent{ID: cfg.AgentID, UserID: cfg.AgentUserID, OrgID: cfg.AgentOrgID, Name: cfg.AgentName}
	if _, err := expansionCreator.CreatePendingInlineExpansion(req.Context(), agentForCreate, taskID, &additions, parsed.Reason, expansionRisk); err != nil {
		audit("fallthrough", "inline_expansion_pending_create_failed", err.Error()+"; deferring to dashboard rewrite")
		trace("inline_expansion.pending_create_failed", "err", err.Error(), "task_id", taskID)
		return conversation.ToolUseVerdict{}, false
	}

	now := time.Now().UTC()
	innerHold, holdErr := cfg.PendingApprovals.Hold(req.Context(), PendingLiteApproval{
		UserID:                   cfg.AgentUserID,
		AgentID:                  cfg.AgentID,
		Provider:                 provider,
		ConversationID:           cfg.ConversationID,
		ToolUse:                  tu,
		Reason:                   "inline expansion awaiting user approval",
		Stage:                    StageAwaitingExpansionApproval,
		ExpansionTaskID:          taskID,
		ExpansionAdditions:       &additions,
		ExpansionReason:          parsed.Reason,
		ExpansionPrecomputedRisk: expansionRisk,
		ScopeDriftID:             parsed.DriftID,
		CreatedAt:                now,
		ExpiresAt:                now.Add(inlineTaskApprovalHoldTTL),
	})
	if holdErr != nil {
		// Cache hold failed — roll the pending expansion back so the
		// dashboard doesn't show an orphan pending-scope-expansion row
		// whose chat anchor never existed.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 5*time.Second)
		_ = expansionCreator.ExpireInlineExpansion(rollbackCtx, taskID, cfg.AgentUserID)
		cancel()
		audit("fallthrough", "inline_expansion_hold_failed", holdErr.Error()+"; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}
	if innerHold.Evicted != nil {
		// LRU evicted an older hold. If that hold was an
		// expansion-approval hold itself, expire its DB anchor for the
		// same reason CleanupEvictedInlineTask does for task creation
		// — otherwise the dashboard keeps showing "reply in chat"
		// guidance for a hold the cache no longer carries.
		cleanupEvictedInlineExpansion(req.Context(), cfg, innerHold.Evicted)
	}

	audit("approve", "pending", "inline_expansion_pending_approval: awaiting user yes/no on expand")
	trace("inline_expansion.held",
		"approval_id", innerHold.Pending.ID,
		"task_id", taskID,
		"signal", "query",
	)
	// Audit the in-flight expand into task_lifecycle_events so the
	// body editor on the next turn can reconstruct the model's
	// missing assistant turn after the substituted-prompt strip
	// (without the original tool_use the model has no record of
	// having called expand and re-emits it). Best-effort: a store
	// outage here must not strand the hold.
	if payload, marshalErr := json.Marshal(map[string]any{
		"expected_tools":       additions.ExpectedTools,
		"expected_egress":      additions.ExpectedEgress,
		"required_credentials": additions.RequiredCredentials,
		"reason":               parsed.Reason,
	}); marshalErr == nil {
		logTaskLifecycleEventFromHold(req.Context(), taskLifecycleAuditCtx{
			st:             cfg.Store,
			trace:          trace,
			userID:         cfg.AgentUserID,
			agentID:        cfg.AgentID,
			conversationID: cfg.ConversationID,
			requestID:      cfg.RequestID,
		}, taskID, innerHold.Pending.ID, store.TaskLifecycleEventTaskExpandPending, "inline_chat", tu, payload)
	}
	promptText := renderExpansionApprovalPrompt(&additions, parsed.Reason, parentPurpose, taskID, parentLifetime, expansionRisk, innerHold.Pending.ID)
	verdict := conversation.ToolUseVerdict{
		Allowed:        false,
		Reason:         "Clawvisor: awaiting inline scope-expansion approval",
		SubstituteWith: promptText,
		HeldKindHint:   "approval",
	}
	// AskUserQuestion substitution mirrors the task-creation path:
	// when the harness declares the picker tool, emit the prompt as
	// a text block AND a synthetic AskUserQuestion call so the user
	// gets a click UI instead of typing yes/no. The picker question
	// is expansion-specific ("Approve this scope expansion?") but
	// the call shape is shared via buildAskUserQuestionApprovalCall.
	useAskUserQuestion := askUserQuestionAvailable(cfg.AvailableTools)
	trace("inline_expansion.substitution_shape",
		"approval_id", innerHold.Pending.ID,
		"shape", inlineSubstitutionShape(useAskUserQuestion),
		"ask_user_question_present", useAskUserQuestion,
		"available_tool_count", len(cfg.AvailableTools),
	)
	if useAskUserQuestion {
		verdict.SubstituteWithToolCall = buildAskUserQuestionApprovalCall(innerHold.Pending.ID, askUserQuestionApprovalSpec{
			Question:       "Approve this scope expansion?",
			Header:         "Expand task",
			YesDescription: "Authorize the additional scope",
		})
	}
	guard.Success()
	return verdict, true
}

// assessInlineExpansionRisk is the expansion mirror of
// assessInlineTaskRisk: when cfg.TaskRiskAssessor is configured and
// returns a usable verdict, the LLM read is the truth and is
// returned as-is. The deterministic envelope-shape policy is the
// FALLBACK ONLY — used when the assessor is unconfigured, errors,
// or returns "unknown" — so the prompt still carries a level. The
// floor does NOT act as a backstop that can raise the LLM read.
//
// This deliberately mirrors assessInlineTaskRisk so the two inline
// surfaces (creation + expansion) behave identically. The headless
// paths (Create and Expand handlers) still merge LLM + floor for an
// audit-grade safety net — the asymmetry is intentional: inline has
// the user in the loop seeing the prompt, so the LLM verdict
// directly drives what they consent to.
func assessInlineExpansionRisk(req *http.Request, cfg PostprocessConfig, parentPurpose string, merged runtimetasks.Envelope, trace func(event string, kv ...any)) *taskrisk.RiskAssessment {
	if cfg.TaskRiskAssessor == nil {
		return runtimepolicy.AssessTaskEnvelope(parentPurpose, merged)
	}
	llmVerdict := cfg.TaskRiskAssessor.AssessEnvelope(req.Context(), TaskRiskAssessRequest{
		Purpose:                parentPurpose,
		AgentName:              cfg.AgentName,
		UserID:                 cfg.AgentUserID,
		OrgID:                  cfg.AgentOrgID,
		ExpectedTools:          merged.ExpectedTools,
		ExpectedEgress:         merged.ExpectedEgress,
		RequiredCredentials:    merged.RequiredCredentials,
		IntentVerificationMode: merged.IntentVerificationMode,
		ExpectedUse:            merged.ExpectedUse,
	})
	if llmVerdict == nil || strings.EqualFold(strings.TrimSpace(llmVerdict.RiskLevel), "unknown") {
		trace("inline_expansion.risk_llm_unavailable")
		return runtimepolicy.AssessTaskEnvelope(parentPurpose, merged)
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

// cleanupEvictedInlineExpansion mirrors CleanupEvictedInlineTask for
// expansion-stage holds. Called by the intercept when a fresh Hold
// displaces an older inline-expansion hold. Without this the
// dashboard would keep showing the row as pending_scope_expansion
// with a "reply in chat" notice the cache can no longer resolve.
func cleanupEvictedInlineExpansion(ctx context.Context, cfg PostprocessConfig, evicted *PendingLiteApproval) {
	if evicted == nil || evicted.Stage != StageAwaitingExpansionApproval {
		return
	}
	if evicted.ExpansionTaskID == "" || evicted.UserID == "" {
		return
	}
	expansionCreator, ok := cfg.InlineTaskCreator.(InlineExpansionCreator)
	if !ok || expansionCreator == nil {
		return
	}
	expireCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := expansionCreator.ExpireInlineExpansion(expireCtx, evicted.ExpansionTaskID, evicted.UserID); err != nil && cfg.Trace != nil {
		cfg.Trace.Emit(map[string]any{
			"event":       "inline_expansion.evicted_expire_failed",
			"request_id":  cfg.RequestID,
			"user_id":     evicted.UserID,
			"agent_id":    evicted.AgentID,
			"approval_id": evicted.ID,
			"task_id":     evicted.ExpansionTaskID,
			"err":         err.Error(),
		})
	}
}
