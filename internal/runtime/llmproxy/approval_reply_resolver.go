package llmproxy

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

type approvalReplyActionKind string

const (
	approvalReplyActionNoop                      approvalReplyActionKind = "noop"
	approvalReplyActionReleaseTool               approvalReplyActionKind = "release_tool"
	approvalReplyActionStartInlineTaskDefinition approvalReplyActionKind = "start_inline_task_definition"
	approvalReplyActionApproveInlineTask         approvalReplyActionKind = "approve_inline_task"
	approvalReplyActionDenyInlineTask            approvalReplyActionKind = "deny_inline_task"
)

type approvalReplyAction struct {
	Kind       approvalReplyActionKind
	Verb       string
	ApprovalID string
	Hold       *PendingLiteApproval
}

type approvalReplyRoutingRequest struct {
	UserID          string
	AgentID         string
	Provider        conversation.Provider
	ConversationID  string
	PendingApproval PendingApprovalCache
	Verb            string
	ApprovalID      string
}

// resolveApprovalReplyAction centralizes the shared routing rule for
// conversational approval replies:
//
//   - explicit approval IDs target only that hold
//   - bare replies target the newest visible hold across stages
//   - yes/no replies normalize to approve/deny
//   - approve/deny on an inline-task hold belongs to the inline rewriter
//   - approve/deny on any other hold belongs to the regular release path
//   - task starts the inline task-definition flow for the targeted hold
//
// This function only peeks and classifies; callers still own the
// side effects for their action.
func resolveApprovalReplyAction(ctx context.Context, req approvalReplyRoutingRequest) (approvalReplyAction, error) {
	action := approvalReplyAction{
		Kind:       approvalReplyActionNoop,
		Verb:       req.Verb,
		ApprovalID: req.ApprovalID,
	}
	if req.PendingApproval == nil || req.UserID == "" || req.AgentID == "" {
		return action, nil
	}
	switch req.Verb {
	case "approve", "deny", "task":
	default:
		return action, nil
	}

	hold, err := req.PendingApproval.Peek(ctx, ResolveRequest{
		UserID:         req.UserID,
		AgentID:        req.AgentID,
		Provider:       req.Provider,
		ConversationID: req.ConversationID,
		ApprovalID:     req.ApprovalID,
	})
	if err != nil {
		return action, err
	}
	if hold == nil {
		return action, nil
	}
	action.Hold = hold

	switch req.Verb {
	case "task":
		action.Kind = approvalReplyActionStartInlineTaskDefinition
	case "approve":
		if hold.Stage == StageAwaitingTaskApproval {
			action.Kind = approvalReplyActionApproveInlineTask
		} else {
			action.Kind = approvalReplyActionReleaseTool
		}
	case "deny":
		if hold.Stage == StageAwaitingTaskApproval {
			action.Kind = approvalReplyActionDenyInlineTask
		} else {
			action.Kind = approvalReplyActionReleaseTool
		}
	}
	return action, nil
}
