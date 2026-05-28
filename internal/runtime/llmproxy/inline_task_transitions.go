package llmproxy

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

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
	rewritten, ok, err := editor.ReplaceLatestUserText("task", pendingApprovalID, taskCreationPromptForHolds(pending.AllHolds()))
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
		if hasPending && resolved != nil && resolved.PendingTaskID != "" && req.Agent != nil {
			if err := pendingCreator.DenyInlineTask(ctx, resolved.PendingTaskID, req.Agent.UserID); err != nil {
				out.Reason = "deny failed: " + err.Error()
			}
		}
		out.Decision = "deny"
		out.Outcome = "inline_task_denied"
		if out.Reason == "" {
			out.Reason = "user denied inline task"
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
		created, createErr := pendingCreator.ApproveInlineTask(ctx, resolved.PendingTaskID, req.Agent.UserID)
		if createErr != nil {
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
		out.Decision = "deny"
		out.Outcome = "inline_task_definition_missing"
		out.Reason = "missing task definition and pending task id on approval"
		return renderInlineTaskCreatorErrorReply("missing task definition on approval"), out
	}
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
