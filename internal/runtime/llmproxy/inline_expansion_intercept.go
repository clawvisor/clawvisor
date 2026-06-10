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

	// Land the pending state in the DB before holding so the dashboard
	// sees the in-flight expansion as a pending row even while the
	// chat anchor owns the decide window. The creator runs the same
	// derived-action + credential gates the public Expand handler
	// uses, so any failure path here is identical in shape to the
	// headless deny — same error text the agent would have gotten.
	agentForCreate := &store.Agent{ID: cfg.AgentID, UserID: cfg.AgentUserID, Name: cfg.AgentName}
	if _, err := expansionCreator.CreatePendingInlineExpansion(req.Context(), agentForCreate, taskID, &additions, parsed.Reason); err != nil {
		audit("fallthrough", "inline_expansion_pending_create_failed", err.Error()+"; deferring to dashboard rewrite")
		trace("inline_expansion.pending_create_failed", "err", err.Error(), "task_id", taskID)
		return conversation.ToolUseVerdict{}, false
	}

	// Fetch the parent task's purpose + lifetime AND derive the
	// merged-envelope risk for the prompt. Best-effort lookup — on
	// failure the prompt renders with what we have. Lifetime is what
	// triggers the "standing (no expiry)" callout for the higher-
	// blast-radius case; risk is the merged-envelope reassessment
	// (same shape ExpandApprove writes to the row on approve) so the
	// reviewer sees the same level they'll get post-approve, not
	// just the parent's pre-expansion level.
	parentPurpose := ""
	parentLifetime := ""
	var expansionRisk *taskrisk.RiskAssessment
	if cfg.Store != nil {
		if parent, err := cfg.Store.GetTask(req.Context(), taskID); err == nil && parent != nil {
			parentPurpose = parent.Purpose
			parentLifetime = parent.Lifetime
			if parentEnv, envErr := runtimetasks.EnvelopeFromTask(parent); envErr == nil {
				merge := runtimetasks.MergeEnvelopes(parentEnv, additions)
				if assessment := runtimepolicy.AssessTaskEnvelope(parent.Purpose, merge.Merged); assessment != nil && strings.TrimSpace(assessment.RiskLevel) != "" {
					expansionRisk = assessment
				}
			}
		}
	}

	now := time.Now().UTC()
	innerHold, holdErr := cfg.PendingApprovals.Hold(req.Context(), PendingLiteApproval{
		UserID:             cfg.AgentUserID,
		AgentID:            cfg.AgentID,
		Provider:           provider,
		ConversationID:     cfg.ConversationID,
		ToolUse:            tu,
		Reason:             "inline expansion awaiting user approval",
		Stage:              StageAwaitingExpansionApproval,
		ExpansionTaskID:    taskID,
		ExpansionAdditions: &additions,
		ExpansionReason:    parsed.Reason,
		CreatedAt:          now,
		ExpiresAt:          now.Add(inlineTaskApprovalHoldTTL),
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
	return verdict, true
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
