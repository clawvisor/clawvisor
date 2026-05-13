package llmproxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// InlineApprovalRewriteRequest is the input to
// RewriteInlineTaskApprovalReply. Parallel shape to
// TaskReplyRewriteRequest — both run in the request preprocess
// before the LLM call.
type InlineApprovalRewriteRequest struct {
	HTTPRequest     *http.Request
	Provider        conversation.Provider
	Body            []byte
	Agent           *store.Agent
	PendingApproval PendingApprovalCache
	// Creator is the handlers-side helper that creates the task in
	// the store with surface=inline_chat. Required for approve;
	// optional for deny (deny doesn't touch the store).
	Creator InlineTaskCreator
	Audit   *AuditEmitter
	// RequestID is forwarded into the audit row produced when the
	// rewrite resolves an inline task.
	RequestID string
}

// InlineApprovalRewriteResult reports what happened. When Rewritten
// is true, the body has been replaced and the request should flow to
// the LLM with the new body. The Decision/Outcome/Reason fields go
// into the handler's audit_params so the audit row records the
// inline gesture.
type InlineApprovalRewriteResult struct {
	Body      []byte
	Rewritten bool
	// Decision is "allow" on a successful approve, "deny" on any
	// failure path (deny verb, missing creator, creator error).
	Decision string
	// Outcome is the short audit-event tag.
	Outcome string
	// Reason is the human-readable explanation included in the audit
	// row when something went wrong. Empty on success.
	Reason string
	// TaskID is the created task's ID on a successful approve.
	TaskID string
	// ApprovalRecordID is the canonical approval_records row id
	// created at the same time as the task. Useful for audit traces.
	ApprovalRecordID string
}

// RewriteInlineTaskApprovalReply consumes an awaiting_task_approval
// hold when the user's most recent message is "approve" or "deny",
// creates the task (on approve), drops the linked outer tool hold,
// and rewrites the user message to include task-creation context.
//
// This replaces the prior synth-tool_use approach. By rewriting the
// user message and letting the request flow to the LLM, we:
//
//   - Avoid fabricating an assistant tool_use the LLM never authored
//     (which previously confused the model into re-POSTing /control/tasks).
//   - Avoid spoofing the harness into running shell commands the
//     model didn't actually emit.
//   - Give the LLM a clean conversation state with explicit context
//     ("task X created and active; proceed; do NOT re-POST").
//
// When the user's hold isn't an inline-task hold (e.g. a regular
// tool-stage approval), this returns (body, Rewritten=false, nil)
// and the existing TryReleasePendingApproval handles it unchanged.
func RewriteInlineTaskApprovalReply(ctx context.Context, req InlineApprovalRewriteRequest) (InlineApprovalRewriteResult, error) {
	if req.PendingApproval == nil || req.Agent == nil {
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}
	verb, approvalID := conversation.ApprovalReplyForProvider(req.Provider, req.Body)
	if verb != "approve" && verb != "deny" {
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}

	inner, err := req.PendingApproval.Peek(ctx, ResolveRequest{
		UserID:     req.Agent.UserID,
		AgentID:    req.Agent.ID,
		Provider:   req.Provider,
		ApprovalID: approvalID,
	})
	if err != nil {
		return InlineApprovalRewriteResult{Body: req.Body}, err
	}
	if inner == nil || inner.Stage != StageAwaitingTaskApproval {
		// Either no hold or it's a tool-stage hold; let the regular
		// TryReleasePendingApproval path handle it.
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}

	// Consume the inner hold so TryReleasePendingApproval doesn't
	// double-handle it.
	resolved, err := req.PendingApproval.Resolve(ctx, ResolveRequest{
		UserID:     req.Agent.UserID,
		AgentID:    req.Agent.ID,
		Provider:   req.Provider,
		ApprovalID: inner.ID,
	})
	if err != nil {
		return InlineApprovalRewriteResult{Body: req.Body}, err
	}
	if resolved == nil {
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}

	// Drop the linked outer tool hold so it doesn't sit in the cache
	// re-matching subsequent approval prompts. The model will re-emit
	// the original tool naturally now that the task scope covers it.
	if resolved.AwaitingTaskFor != "" {
		_ = req.PendingApproval.Drop(ctx, ResolveRequest{
			UserID:     req.Agent.UserID,
			AgentID:    req.Agent.ID,
			Provider:   req.Provider,
			ApprovalID: resolved.AwaitingTaskFor,
		})
	}

	var replacement string
	out := InlineApprovalRewriteResult{Body: req.Body}

	if verb == "deny" {
		replacement = renderInlineTaskDenyReply()
		out.Decision = "deny"
		out.Outcome = "inline_task_denied"
		out.Reason = "user denied inline task"
	} else {
		// approve
		switch {
		case req.Creator == nil:
			replacement = renderInlineTaskCreatorErrorReply("inline task creation is not available on this daemon")
			out.Decision = "deny"
			out.Outcome = "inline_task_creator_missing"
			out.Reason = "no inline task creator configured"
		case resolved.TaskDefinition == nil:
			replacement = renderInlineTaskCreatorErrorReply("missing task definition on approval")
			out.Decision = "deny"
			out.Outcome = "inline_task_definition_missing"
			out.Reason = "missing task definition on approval"
		default:
			originalToolUseID := resolved.AwaitingTaskFor
			created, createErr := req.Creator.CreateInlineApprovedTask(ctx, req.Agent, resolved.TaskDefinition, originalToolUseID)
			if createErr != nil {
				replacement = renderInlineTaskCreatorErrorReply(createErr.Error())
				out.Decision = "deny"
				out.Outcome = "inline_task_create_failed"
				out.Reason = "create failed: " + createErr.Error()
			} else {
				replacement = renderInlineTaskApprovedReply(created)
				out.Decision = "allow"
				out.Outcome = "inline_task_approved"
				out.TaskID = created.ID
				out.ApprovalRecordID = created.ApprovalRecordID
				if req.Audit != nil {
					req.Audit.LogInlineTaskApproved(ctx, req.Agent, req.RequestID, resolved, created)
				}
			}
		}
	}

	rewritten, ok, err := replaceApprovalReplyForProvider(req.HTTPRequest, req.Provider, req.Body, verb, replacement)
	if err != nil {
		return out, err
	}
	if !ok {
		// Couldn't rewrite (unsupported provider or unexpected body
		// shape). Hold is already consumed; return the original body
		// but mark the outcome so the audit row records what happened.
		return out, nil
	}
	out.Body = rewritten
	out.Rewritten = true
	return out, nil
}

// renderInlineTaskApprovedReply is the user-message text the LLM
// sees in place of bare "approve cv-xxx". The bracketed context tells
// the model the task is active and that it should proceed without
// re-POSTing /control/tasks.
func renderInlineTaskApprovedReply(t *InlineApprovedTask) string {
	if t == nil {
		return "approve\n\n[Clawvisor: inline task created and active. Proceed with the originally requested work; do NOT POST /control/tasks again — that would create a duplicate task.]"
	}
	lifetime := strings.TrimSpace(t.Lifetime)
	if lifetime == "" {
		lifetime = "session"
	}
	purpose := strings.TrimSpace(t.Purpose)
	if purpose == "" {
		purpose = "(unspecified)"
	}
	return fmt.Sprintf("approve\n\n[Clawvisor: inline task %s created and active. Approval source: inline_chat. Lifetime: %s. Purpose: %s. The task covers the originally requested work; proceed by emitting your next tool_use(s). Do NOT POST /control/tasks again — that would create a duplicate task.]",
		t.ID, lifetime, purpose)
}

// renderInlineTaskDenyReply is the user-message text the LLM sees
// in place of bare "deny cv-xxx". Tells the model to stop and not
// retry the task-creation request.
func renderInlineTaskDenyReply() string {
	return "deny\n\n[Clawvisor: the user denied the task-creation request. Do not retry. Acknowledge the denial; stop unless the user issues a new request.]"
}

// renderInlineTaskCreatorErrorReply is used when the user approved
// but the task could not be created (validation error, store error,
// missing creator wiring). The LLM should treat this as a denial and
// surface the failure to the user.
func renderInlineTaskCreatorErrorReply(msg string) string {
	return fmt.Sprintf("deny\n\n[Clawvisor: inline task creation failed — %s. Acknowledge the failure to the user; do not retry without changes.]", msg)
}
