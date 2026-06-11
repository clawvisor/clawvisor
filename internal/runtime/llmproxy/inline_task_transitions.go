package llmproxy

import (
	"context"
	"errors"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// taskLifecycleAuditCtxFromRewrite pulls the audit context out of the
// rewrite request so resolution-time event writes can run from the
// same call site as the side-effect dispatch. Wraps the store +
// trace + scoping IDs into the struct the writer expects. Returns a
// zero ctx (st nil) when the request has no store wired, which makes
// the writer skip silently — that path triggers in unit tests that
// fake the rewrite without a backing store.
func taskLifecycleAuditCtxFromRewrite(req InlineApprovalRewriteRequest) taskLifecycleAuditCtx {
	userID := ""
	agentID := ""
	if req.Agent != nil {
		userID = req.Agent.UserID
		agentID = req.Agent.ID
	}
	return taskLifecycleAuditCtx{
		st:             req.Store,
		trace:          req.Trace,
		userID:         userID,
		agentID:        agentID,
		conversationID: req.ConversationID,
		requestID:      req.RequestID,
	}
}

func startInlineTaskDefinition(ctx context.Context, req TaskReplyRewriteRequest, action approvalReplyAction, editor approvalBodyEditor) (TaskReplyRewriteResult, error) {
	// Conversation scope flows through to the cache so the consumed hold
	// is picked from this conversation's bucket, even when another
	// conversation sharing the token has its own pending holds.
	// Drop the original tool hold. The user typed "task" so the
	// harness now shows the task-creation prompt instead — there's
	// no way back to approving the original tool. Leaving the hold
	// in the cache was a latent safety issue: if the model didn't
	// follow through with POST /api/control/tasks, the orphan hold could
	// later be resolved as a regular tool approval by a bare approve.
	//
	// The inline-task intercept now relies on the query signal
	// (surface=inline in the URL) rather than a retained
	// awaiting_task_definition hold. taskCreationPrompt always renders
	// surface=inline in the example URL, so compliant models still
	// drive the inline path.
	pending, err := consumeApprovalActionHold(ctx, req.PendingApproval, req.Agent, req.Provider, req.ConversationID, action)
	if err != nil || pending == nil {
		return TaskReplyRewriteResult{Body: req.Body}, err
	}
	pendingApprovalID := ""
	if pending != nil {
		pendingApprovalID = pending.ID
	}
	// For a coalesced hold, generate a task definition prompt that
	// covers every held tool_use — not just the primary. Otherwise
	// the user typing "task" on a multi-call review would scope only
	// one call and the sibling reviewed calls re-prompt on retry,
	// defeating the point of the gesture. Single-tool holds collapse
	// to the legacy single-element prompt unchanged.
	rewritten, ok, err := editor.ReplaceLatestUserText("task", pendingApprovalID, TaskCreationPromptForHolds(pending.AllHolds()), nil)
	if err != nil || !ok {
		return TaskReplyRewriteResult{Body: req.Body}, err
	}
	return TaskReplyRewriteResult{Body: rewritten, Rewritten: true}, nil
}

func consumeApprovalActionHold(ctx context.Context, cache PendingApprovalCache, agent *store.Agent, provider conversation.Provider, conversationID string, action approvalReplyAction) (*PendingLiteApproval, error) {
	if cache == nil || agent == nil || action.Hold == nil {
		return nil, nil
	}
	return cache.Resolve(ctx, ResolveRequest{
		UserID:         agent.UserID,
		AgentID:        agent.ID,
		Provider:       provider,
		ConversationID: conversationID,
		ApprovalID:     action.Hold.ID,
	})
}

func dropLinkedToolHold(ctx context.Context, cache PendingApprovalCache, agent *store.Agent, provider conversation.Provider, conversationID string, resolved *PendingLiteApproval) {
	if cache == nil || agent == nil || resolved == nil || resolved.AwaitingTaskFor == "" {
		return
	}
	_ = cache.Drop(ctx, ResolveRequest{
		UserID:         agent.UserID,
		AgentID:        agent.ID,
		Provider:       provider,
		ConversationID: conversationID,
		ApprovalID:     resolved.AwaitingTaskFor,
	})
}

func resolveInlineTaskApproval(ctx context.Context, req InlineApprovalRewriteRequest, resolved *PendingLiteApproval, verb string) (string, InlineApprovalRewriteResult) {
	out := InlineApprovalRewriteResult{Body: req.Body}

	// Deny is a guaranteed no-op success regardless of creator or
	// resolved-hold state. The user typed "deny" — the model must see
	// a denial reply, period. Surfacing "creator missing" or
	// "definition missing" instead would tell the model that its
	// approval failed for some operational reason when in fact the
	// user explicitly rejected it. Optimistically transition the
	// pending task when we have everything we need; missing pieces
	// just skip the side effect and still render the denial.
	pendingCreator, hasPending := req.Creator.(InlineTaskPendingCreator)
	if verb == "deny" {
		out.Decision = "deny"
		out.Outcome = "inline_task_denied"
		out.Reason = "user denied inline task"
		denySideEffectLanded := false
		if hasPending && resolved != nil && resolved.PendingTaskID != "" && req.Agent != nil {
			if err := pendingCreator.DenyInlineTask(ctx, resolved.PendingTaskID, req.Agent.UserID); err != nil {
				// Distinct outcome on rollback error so audit
				// dashboards can spot a denial whose DB-side
				// transition failed without scanning the reason
				// string. The user-facing reply is still a
				// denial — the user's decision stands; the side
				// effect just didn't land.
				out.Outcome = "inline_task_denied_with_rollback_error"
				out.Reason = "user denied inline task; deny side-effect failed: " + err.Error()
			} else {
				denySideEffectLanded = true
			}
		}
		// Only log the *_denied lifecycle event when the deny side
		// effect actually landed. Otherwise the recovery path could
		// read a "denied" event for a task that's still pending,
		// causing audit/state split-brain on next-turn restart.
		if denySideEffectLanded {
			logTaskLifecycleEventResolution(ctx, taskLifecycleAuditCtxFromRewrite(req),
				resolved.PendingTaskID, resolved.ID,
				store.TaskLifecycleEventTaskCreateDenied, "inline_chat", nil)
		}
		return renderInlineTaskDenyReply(), out
	}

	// Approve path needs both a creator and a resolved hold.
	switch {
	case req.Creator == nil:
		out.Decision = "deny"
		out.Outcome = "inline_task_creator_missing"
		out.Reason = "no inline task creator configured"
		return renderInlineTaskCreatorErrorReply("inline task creation is not available on this daemon"), out
	case resolved == nil:
		out.Decision = "deny"
		out.Outcome = "inline_task_definition_missing"
		out.Reason = "missing approval hold on resolve"
		return renderInlineTaskCreatorErrorReply("missing approval hold on resolve"), out
	}

	// Prefer the new pending-task flow: the intercept already landed a
	// store.Task at pending_approval; here we just transition it to
	// active. Legacy creators (test stubs without the extension) still
	// drive the original create-on-approve path so we don't break
	// those callers.
	switch {
	case hasPending && resolved.PendingTaskID != "" && req.Agent != nil:
		logTaskLifecycleEventResolution(ctx, taskLifecycleAuditCtxFromRewrite(req),
			resolved.PendingTaskID, resolved.ID,
			store.TaskLifecycleEventTaskCreateApproved, "inline_chat", nil)
		created, createErr := pendingCreator.ApproveInlineTask(ctx, resolved.PendingTaskID, req.Agent.UserID)
		if createErr != nil {
			// Distinct branch when the task was already terminated
			// from a non-chat surface (dashboard or notifier Deny,
			// the 24h expiry sweep, manual revocation). The user
			// has already dismissed this work; we render an
			// explanatory reply so the model understands it lost a
			// race rather than treating it as a generic creator
			// failure that might prompt a retry of the same body.
			var terminal *ErrInlineTaskAlreadyTerminal
			if errors.As(createErr, &terminal) {
				out.Decision = "deny"
				out.Outcome = "inline_task_already_terminal"
				out.Reason = "task already " + terminal.Status + " from another surface before approve landed"
				return renderInlineTaskAlreadyTerminalReply(terminal.Status), out
			}
			out.Decision = "deny"
			out.Outcome = "inline_task_create_failed"
			out.Reason = "approve failed: " + createErr.Error()
			return renderInlineTaskCreatorErrorReply(createErr.Error()), out
		}
		return finalizeInlineApproval(ctx, req, resolved, created, &out)

	case resolved.TaskDefinition != nil:
		// Legacy fall-through: creator doesn't expose the pending
		// flow OR the hold predates the intercept's pending-task
		// landing. Create-with-precomputed (or plain create) to
		// keep older callers working until they migrate.
		originalToolUseID := resolved.AwaitingTaskFor
		var created *InlineApprovedTask
		var createErr error
		if withAssessment, ok := req.Creator.(InlineTaskCreatorWithAssessment); ok {
			created, createErr = withAssessment.CreateInlineApprovedTaskWithAssessment(ctx, req.Agent, resolved.TaskDefinition, originalToolUseID, resolved.PrecomputedRisk)
		} else {
			created, createErr = req.Creator.CreateInlineApprovedTask(ctx, req.Agent, resolved.TaskDefinition, originalToolUseID)
		}
		if createErr != nil {
			out.Decision = "deny"
			out.Outcome = "inline_task_create_failed"
			out.Reason = "create failed: " + createErr.Error()
			return renderInlineTaskCreatorErrorReply(createErr.Error()), out
		}
		return finalizeInlineApproval(ctx, req, resolved, created, &out)

	default:
		// Reached when neither the pending-flow shape (creator
		// implements InlineTaskPendingCreator AND resolved.PendingTaskID
		// is set AND req.Agent is non-nil) nor the legacy shape
		// (resolved.TaskDefinition is non-nil) is satisfied. Common
		// causes: a nil Agent on the request, or a hold rebuilt from
		// state that lost both linkages.
		out.Decision = "deny"
		out.Outcome = "inline_task_definition_missing"
		out.Reason = "approve flow preconditions not met (creator/agent/pending task id/task def)"
		return renderInlineTaskCreatorErrorReply("missing approval context on approval"), out
	}
}

// resolveInlineExpansionApproval is the expansion analogue of
// resolveInlineTaskApproval. The chat-side approve/deny verb resolves
// the existing pending-scope-expansion row through the
// InlineExpansionCreator interface (which TasksHandler implements
// via type assertion on the same field as the task creator). The
// returned substitution text is what replaces the user's "approve" /
// "deny" turn so the model sees the verdict in context.
func resolveInlineExpansionApproval(ctx context.Context, req InlineApprovalRewriteRequest, resolved *PendingLiteApproval, verb string) (string, InlineApprovalRewriteResult) {
	out := InlineApprovalRewriteResult{Body: req.Body}

	expansionCreator, hasExpansion := req.Creator.(InlineExpansionCreator)
	// Deny is the same shape as inline-task deny: guaranteed user-facing
	// success, optimistic side effect on the pending row. Side-effect
	// failure surfaces in the outcome name without flipping the
	// user-facing reply (the user denied; the row's eventual cleanup is
	// an operator concern).
	if verb == "deny" {
		out.Decision = "deny"
		out.Outcome = "inline_expansion_denied"
		out.Reason = "user denied inline expansion"
		denySideEffectLanded := false
		if hasExpansion && resolved != nil && resolved.ExpansionTaskID != "" && req.Agent != nil {
			if err := expansionCreator.DenyInlineExpansion(ctx, resolved.ExpansionTaskID, req.Agent.UserID); err != nil {
				out.Outcome = "inline_expansion_denied_with_rollback_error"
				out.Reason = "user denied inline expansion; deny side-effect failed: " + err.Error()
			} else {
				denySideEffectLanded = true
			}
		}
		// Only log the *_denied lifecycle event when the deny side
		// effect landed. A logged-denied audit row for a task still
		// in pending_scope_expansion would mislead the recovery path
		// (lifecycle-recovered approve→deny becomes a no-op when the
		// real task is still pending and recoverable).
		if denySideEffectLanded {
			logTaskLifecycleEventResolution(ctx, taskLifecycleAuditCtxFromRewrite(req),
				resolved.ExpansionTaskID, resolved.ID,
				store.TaskLifecycleEventTaskExpandDenied, "inline_chat", nil)
		}
		return renderInlineExpansionDenyReply(), out
	}

	switch {
	case !hasExpansion || expansionCreator == nil:
		out.Decision = "deny"
		out.Outcome = "inline_expansion_creator_missing"
		out.Reason = "no inline expansion creator configured"
		return renderInlineExpansionCreatorErrorReply("inline scope expansion is not available on this daemon"), out
	case resolved == nil:
		out.Decision = "deny"
		out.Outcome = "inline_expansion_definition_missing"
		out.Reason = "missing approval hold on resolve"
		return renderInlineExpansionCreatorErrorReply("missing approval hold on resolve"), out
	case resolved.ExpansionTaskID == "":
		out.Decision = "deny"
		out.Outcome = "inline_expansion_definition_missing"
		out.Reason = "approval hold missing expansion task id"
		return renderInlineExpansionCreatorErrorReply("approval hold missing expansion context"), out
	case req.Agent == nil:
		out.Decision = "deny"
		out.Outcome = "inline_expansion_agent_missing"
		out.Reason = "missing agent on approval"
		return renderInlineExpansionCreatorErrorReply("missing agent on approval"), out
	}

	logTaskLifecycleEventResolution(ctx, taskLifecycleAuditCtxFromRewrite(req),
		resolved.ExpansionTaskID, resolved.ID,
		store.TaskLifecycleEventTaskExpandApproved, "inline_chat", nil)
	expanded, createErr := expansionCreator.ApproveInlineExpansion(ctx, resolved.ExpansionTaskID, req.Agent.UserID)
	if createErr != nil {
		var terminal *ErrInlineExpansionAlreadyTerminal
		if errors.As(createErr, &terminal) {
			out.Decision = "deny"
			out.Outcome = "inline_expansion_already_terminal"
			out.Reason = "expansion already " + terminal.Status + " from another surface before approve landed"
			return renderInlineExpansionAlreadyTerminalReply(terminal.Status), out
		}
		out.Decision = "deny"
		out.Outcome = "inline_expansion_approve_failed"
		out.Reason = "approve failed: " + createErr.Error()
		return renderInlineExpansionCreatorErrorReply(createErr.Error()), out
	}

	out.Decision = "allow"
	out.Outcome = "inline_expansion_approved"
	out.TaskID = expanded.TaskID
	out.ApprovalRecordID = expanded.ApprovalRecordID
	out.Credentials = expanded.Credentials
	// Note: we do NOT call Checkouts.Set here — the parent task was
	// likely already the agent's active task (we just expanded it),
	// and overwriting an existing checkout from a sibling
	// conversation would silently steal focus on the next call. The
	// caller can re-checkout explicitly via /control/task/checkout if
	// they want to switch.
	if req.Audit != nil {
		// Audit-trail symmetry with the task-creation path
		// (LogInlineTaskApproved): without this row, dashboards
		// filtering by surface=inline_chat see no per-event signal
		// that an expansion (vs. a fresh task creation) was the
		// gesture.
		req.Audit.LogInlineExpansionApproved(ctx, req.Agent, req.RequestID, resolved, expanded)
	}
	return inlineExpansionApprovedReplyAugmentationContextWithMintStatus(
		expanded.TaskID, expanded.Credentials, expanded.CredentialMintFailed,
	), out
}

// finalizeInlineApproval performs the post-approval bookkeeping shared
// by the pending-task and legacy create-on-approve paths: checkout,
// audit emission, return shape. Mutates `out` and returns the
// substituted reply text.
func finalizeInlineApproval(ctx context.Context, req InlineApprovalRewriteRequest, resolved *PendingLiteApproval, created *InlineApprovedTask, out *InlineApprovalRewriteResult) (string, InlineApprovalRewriteResult) {
	out.Decision = "allow"
	out.Outcome = "inline_task_approved"
	out.TaskID = created.ID
	out.ApprovalRecordID = created.ApprovalRecordID
	out.Credentials = created.Credentials
	if req.Checkouts != nil && req.Agent != nil && created.ID != "" {
		if err := req.Checkouts.Set(ctx, TaskCheckoutKey{
			UserID:         req.Agent.UserID,
			AgentID:        req.Agent.ID,
			ConversationID: req.ConversationID,
		}, created.ID, 0); err == nil {
			out.CheckedOut = true
		}
	}
	if req.Audit != nil {
		req.Audit.LogInlineTaskApproved(ctx, req.Agent, req.RequestID, resolved, created)
	}
	// Use the SAME text the persistent augmenter produces on
	// subsequent turns. One canonical rendering avoids showing the
	// model the same user approve turn with different content across
	// calls.
	return inlineApprovedReplyAugmentationContext(created.ID, out.CheckedOut, created.Credentials), *out
}
