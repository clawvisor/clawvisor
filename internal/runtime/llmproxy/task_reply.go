package llmproxy

import (
	"context"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type TaskReplyRewriteRequest struct {
	HTTPRequest     *http.Request
	Provider        conversation.Provider
	Body            []byte
	Agent           *store.Agent
	PendingApproval PendingApprovalCache
}

type TaskReplyRewriteResult struct {
	Body      []byte
	Rewritten bool
}

func RewriteTaskApprovalReply(ctx context.Context, req TaskReplyRewriteRequest) (TaskReplyRewriteResult, error) {
	editor, ok := newApprovalBodyEditor(req.HTTPRequest, req.Provider, req.Body)
	if !ok {
		return TaskReplyRewriteResult{Body: req.Body}, nil
	}
	verb, approvalID, ok := editor.LatestApprovalReply()
	if !ok || verb != "task" || req.PendingApproval == nil || req.Agent == nil {
		return TaskReplyRewriteResult{Body: req.Body}, nil
	}
	action, err := resolveApprovalReplyAction(ctx, approvalReplyRoutingRequest{
		UserID:          req.Agent.UserID,
		AgentID:         req.Agent.ID,
		Provider:        req.Provider,
		PendingApproval: req.PendingApproval,
		Verb:            verb,
		ApprovalID:      approvalID,
	})
	if err != nil || action.Kind != approvalReplyActionStartInlineTaskDefinition || action.Hold == nil {
		return TaskReplyRewriteResult{Body: req.Body}, err
	}
	return startInlineTaskDefinition(ctx, req, action, editor)
}
